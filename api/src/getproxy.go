package main

import (
	"bytes"         // ! для буферизации вывода
	"context"       // ! для отмены команд
	"encoding/json" // ! работа с жсон
	"errors"        // ! для создания простых ошибок
	"fmt"           // ! форматирование вывода
	"log"           // ! логирование
	"net/http"      // ! http клиент
	"net/url"       // ! парсинг VLESS URL
	"os"            // ! работа с файлами и сигналами
	"os/exec"       // ! запуск внешних команд (sing-box)
	"os/signal"     // ! ловим ctrl-c
	"path/filepath" // ! работа с путями
	"strconv"       // ! для конвертации порта в строку
	"strings"       // ! для трима вывода
	"syscall"       // ! системные сигналы (для ctrl-c)
	"time"          // ! таймауты и задержки

	"golang.org/x/net/proxy" // ! чтоб через сокс5 идти для теста
)

const (
	// FIXHARDCODE: вынести урлы в конфиг или флаги
	subscriptionURL = "https://raw.githubusercontent.com/sevcator/5ubscrpt10n/main/mini/m1n1-5ub-27.txt"
	testURL         = "https://www.tiktok.com"

	singBoxBinary   = "../assets/signbox/usr/bin/sing-box" //? путь к sing-box
	finalConfigPath = "/tmp/singbox-final-config.json"

	// * Шаблон для финального конфига с TUN
	finalConfigTemplate = `
{
  "log": {"level": "info", "timestamp": true},
  "dns": {"servers": [{"address": "8.8.8.8"}]},
  "inbounds": [{
      "type": "tun", "tag": "tun-in", "interface_name": "tun0", "inet4_address": "172.19.0.1/30",
      "mtu": 1500, "auto_route": true, "strict_route": true, "stack": "gvisor"
  }],
  "outbounds": [%s, {"type": "direct", "tag": "direct"}],
  "route": {
    "rules": [
      {"protocol": ["tls"], "domain": ["tiktok.com"], "outbound": "proxy"},
      {"network": "udp", "port": 53, "outbound": "direct"}
    ]
  }
}`
	// * шаблон для теста чисто сокс5 и один аутбаунд
	testConfigTemplate = `
{
    "log": {"level": "info", "timestamp": true},
    "inbounds": [{
        "type": "socks",
        "tag": "socks-in",
        "listen": "127.0.0.1",
        "listen_port": 10888
    }],
    "outbounds": [%s]
}`
)

func main() {
	log.Println("Запуск...")

	absSingBoxPath, err := filepath.Abs(singBoxBinary)
	if err != nil {
		log.Fatalf("Ошибка с путем к sing-box: %v", err)
	}
	if _, err := os.Stat(absSingBoxPath); os.IsNotExist(err) {
		log.Fatalf("Бинарник sing-box не найден тут: %s", absSingBoxPath)
	}

	// 1. качаем урлы проксей
	log.Printf("Гружу прокси с %s", subscriptionURL)
	proxyURLs, err := GetProxyURLs(subscriptionURL)
	if err != nil {
		log.Fatalf("Ошибка: %v", err)
	}
	log.Printf("Нашел %d прокси. Ща буду тестить...", len(proxyURLs))

	// 2. ищем рабочий урл
	workingProxyURL, err := findWorkingProxy(proxyURLs, absSingBoxPath)
	if err != nil {
		log.Fatalf("Не нашел рабочий прокси: %v", err)
	}
	log.Println("Нашел живой прокси!")

	// 3. запускаем основной конфиг
	if err := generateAndLaunchFinalConfig(workingProxyURL, absSingBoxPath); err != nil {
		log.Fatalf("Ошибка при финальном запуске: %v", err)
	}
}

// * ищет первый живой прокси тупым перебором
func findWorkingProxy(proxyURLs []string, singBoxPath string) (string, error) {
	for i, proxyURL := range proxyURLs {
		log.Printf("Тест прокси #%d...", i+1)
		if checkProxy(proxyURL, singBoxPath) {
			return proxyURL, nil
		}
	}
	return "", errors.New("все прокси из подписки мертвые")
}

// * главная магия: превращает VLESS URL в JSON для sing-box
func buildVlessOutboundJSON(proxyURL string) (string, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return "", fmt.Errorf("кривой URL: %w", err)
	}

	// вытаскиваем основные части
	uuid := u.User.Username()
	serverHost := u.Hostname()
	serverPort, _ := strconv.Atoi(u.Port())
	if serverPort == 0 {
		serverPort = 443 // дефолтный порт
	}

	// вытаскиваем параметры из ?...
	params := u.Query()
	sni := params.Get("sni")
	if sni == "" {
		sni = serverHost // если sni не указан, используем хост
	}
	transportType := params.Get("type")
	wsPath := params.Get("path")
	wsHost := params.Get("host")
	if wsHost == "" {
		wsHost = sni // если host в ? не указан, берем sni
	}

	// * собираем transport объект
	var transportJSON string
	if transportType == "ws" {
		transportJSON = fmt.Sprintf(
			`,"transport":{"type":"ws","path":"%s","headers":{"Host":"%s"}}`,
			wsPath, wsHost,
		)
	}
	// TODO: добавить поддержку других транспортов (grpc и тд) если надо будет

	// * собираем финальный JSON
	// FIXHARDCODE: tls insecure всегда false, может понадобится менять
	outboundJSON := fmt.Sprintf(`
	{
		"type": "vless",
		"tag": "proxy",
		"server": "%s",
		"server_port": %d,
		"uuid": "%s",
		"tls": {
			"enabled": true,
			"server_name": "%s",
			"insecure": false
		}%s
	}`, serverHost, serverPort, uuid, sni, transportJSON)

	// * проверяем что json валидный
	if !json.Valid([]byte(outboundJSON)) {
		return "", errors.New("собрал кривой json из url")
	}

	return outboundJSON, nil
}

// * запускает сингбокс с одним прокси и стучится на урл для проверки
func checkProxy(proxyURL string, singBoxPath string) bool {
	var stderrBuff bytes.Buffer

	// * делаем json из урла
	outboundJSON, err := buildVlessOutboundJSON(proxyURL)
	if err != nil {
		log.Printf("  -> мимо (не смог собрать json из урла: %v)", err)
		return false
	}

	configContent := fmt.Sprintf(testConfigTemplate, outboundJSON)

	tmpConfigFile, err := os.CreateTemp("", "singbox-test-*.json")
	if err != nil {
		return false
	}
	defer os.Remove(tmpConfigFile.Name())
	tmpConfigFile.WriteString(configContent)
	tmpConfigFile.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, singBoxPath, "run", "-c", tmpConfigFile.Name())
	cmd.Stderr = &stderrBuff

	if err := cmd.Start(); err != nil {
		return false
	}

	time.Sleep(2 * time.Second)

	dialer, err := proxy.SOCKS5("tcp", "127.0.0.1:10888", nil, proxy.Direct)
	if err != nil {
		return false
	}

	httpClient := &http.Client{
		Transport: &http.Transport{DialContext: dialer.(proxy.ContextDialer).DialContext},
		Timeout:   10 * time.Second,
	}

	resp, err := httpClient.Get(testURL)
	if err != nil {
		log.Printf("  -> мимо (ошибка соединения: %v)", err)
		if stderrBuff.Len() > 0 {
			log.Printf("  -> лог sing-box: %s", strings.TrimSpace(stderrBuff.String()))
		}
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		log.Printf("  -> ок (статус: %s)!", resp.Status)
		return true
	}

	log.Printf("  -> мимо (статус: %s)", resp.Status)
	return false
}

// * пишет финальный конфиг и стартует сингбокс через судо
func generateAndLaunchFinalConfig(workingProxyURL string, singBoxPath string) error {
	outboundJSON, err := buildVlessOutboundJSON(workingProxyURL)
	if err != nil {
		return fmt.Errorf("не смог собрать финальный json: %w", err)
	}

	log.Printf("Пишу финальный конфиг в %s", finalConfigPath)
	finalConfigContent := fmt.Sprintf(finalConfigTemplate, outboundJSON)
	os.WriteFile(finalConfigPath, []byte(finalConfigContent), 0644)

	log.Println("=====================================================")
	log.Println("ЗАПУСКАЮ SING-BOX (НУЖЕН SUDO ДЛЯ TUN)")
	log.Println("Жми Ctrl+C чтоб остановить")
	log.Println("=====================================================")

	cmd := exec.Command("sudo", singBoxPath, "run", "-c", finalConfigPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("не смог запустить sing-box: %w", err)
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		log.Println("\nПоймал сигнал, глушу sing-box...")
		sudoProcess := exec.Command("sudo", "kill", fmt.Sprintf("%d", cmd.Process.Pid))
		sudoProcess.Run()
		os.Remove(finalConfigPath)
		os.Exit(0)
	}()

	return cmd.Wait()
}
