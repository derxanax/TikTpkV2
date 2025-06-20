package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"tikpars/proxy"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

// ! URL подписки на прокси
const subscriptionURL = "https://raw.githubusercontent.com/barry-far/V2ray-config/main/Sub1.txt"

// ! URL для скачивания Xray
const xrayDownloadURL = "https://github.com/XTLS/Xray-core/releases/latest/download/Xray-linux-64.zip"
const xrayExecutablePath = "../bin/xray"
const xrayConfigPath = "../bin/config.json"

func main() {
	// 1. Проверяем и если надо, качаем xray
	if err := ensureXray(); err != nil {
		log.Fatalf("FATAL: Failed to get xray binary: %v", err)
	}

	log.Println("Fetching proxy list...")
	configs, err := proxy.GetProxyConfigs(subscriptionURL)
	if err != nil {
		log.Fatalf("FATAL: failed to get proxy configs: %v", err)
	}

	if len(configs) == 0 {
		log.Fatalf("FATAL: no proxy configs found in the subscription")
	}

	log.Printf("Found %d proxy configs. Trying to find a vless proxy...", len(configs))

	var proxyURL string
	for _, p := range configs {
		if strings.HasPrefix(p, "vless://") {
			proxyURL = p
			log.Printf("Found vless proxy: %s", proxyURL)
			break
		}
	}
	if proxyURL == "" {
		log.Println("No vless proxy found, using the very first one.")
		proxyURL = configs[0]
	}

	// 2. Запускаем внешний прокси
	localProxyURL, cleanup, err := startExternalProxy(proxyURL)
	if err != nil {
		log.Fatalf("FATAL: failed to start external proxy: %v", err)
	}
	defer cleanup()

	log.Println("Proxy started successfully. Launching browser...")

	// 3. Запускаем браузер через наш прокси
	browserLauncher := launcher.New().
		Proxy(localProxyURL).
		Headless(false).
		UserDataDir("../session").
		Set("no-sandbox").
		MustLaunch()
	browser := rod.New().ControlURL(browserLauncher).MustConnect()
	defer browser.MustClose() // ! закрываем браузер при выходе

	page := browser.MustPage("https://duckduckgo.com").MustWaitStable()
	log.Printf("Browser opened page: %s", page.MustInfo().Title)

	// 4. Ждем сигнала завершения (Ctrl+C)
	log.Println("Application is running. Press Ctrl+C to exit.")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	log.Println("Shutting down...")
}

// startExternalProxy генерит конфиг для xray, запускает его и возвращает URL прокси
func startExternalProxy(proxyFullURL string) (string, func(), error) {
	parsedURL, err := url.Parse(proxyFullURL)
	if err != nil {
		return "", nil, fmt.Errorf("invalid proxy URL: %w", err)
	}

	// 1. Генерим конфиг для Xray
	listenPort := 10809 // FIXHARDCODE: порт бы тоже свободный искать
	config := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []any{
			map[string]any{
				"port":     listenPort,
				"protocol": "http",
				"listen":   "127.0.0.1",
			},
		},
		"outbounds": []any{
			map[string]any{
				"protocol": parsedURL.Scheme,
				"tag":      "proxy",
				"settings": map[string]any{
					"vnext": []any{
						map[string]any{
							"address": parsedURL.Hostname(),
							"port":    mustParseInt(parsedURL.Port()),
							"users": []any{
								map[string]any{
									"id":         parsedURL.User.Username(),
									"encryption": "none",
								},
							},
						},
					},
				},
				"streamSettings": map[string]any{
					"network": parsedURL.Query().Get("type"),
				},
			},
			map[string]any{"protocol": "freedom", "tag": "direct"},
		},
		"routing": map[string]any{
			"rules": []any{
				map[string]any{
					"type":        "field",
					"outboundTag": "proxy",
					"domain":      []string{"tiktok.com"},
				},
			},
		},
	}

	configBytes, _ := json.MarshalIndent(config, "", "  ")
	if err := os.WriteFile(xrayConfigPath, configBytes, 0644); err != nil {
		return "", nil, fmt.Errorf("failed to write xray config: %w", err)
	}

	// 2. Запускаем xray
	cmd := exec.Command(xrayExecutablePath, "-c", xrayConfigPath)
	cmd.Stderr = os.Stderr // чтоб видеть ошибки от xray
	cmd.Stdout = os.Stdout // и его выхлоп
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("failed to start xray: %w", err)
	}

	// FIX: даем xray долю секунды на запуск, прежде чем пытаться к нему подключиться
	time.Sleep(500 * time.Millisecond)

	log.Printf("Xray process started with PID %d", cmd.Process.Pid)

	cleanup := func() {
		log.Println("Stopping xray process...")
		if err := cmd.Process.Kill(); err != nil {
			log.Printf("Failed to kill xray process: %v", err)
		}
		os.Remove(xrayConfigPath)
	}

	// 3. Возвращаем URL прокси для rod
	return fmt.Sprintf("http://127.0.0.1:%d", listenPort), cleanup, nil
}

// ensureXray проверяет наличие xray и скачивает его при необходимости
func ensureXray() error {
	if _, err := os.Stat(xrayExecutablePath); err == nil {
		log.Println("Xray binary already exists.")
		return nil
	}

	log.Println("Xray binary not found, downloading...")
	binDir := filepath.Dir(xrayExecutablePath)
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return err
	}

	resp, err := http.Get(xrayDownloadURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	zipPath := filepath.Join(binDir, "xray.zip")
	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()
	io.Copy(out, resp.Body)

	log.Println("Unzipping xray...")
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if f.Name == "xray" {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()

			xrayFile, err := os.OpenFile(xrayExecutablePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
			if err != nil {
				return err
			}
			defer xrayFile.Close()

			_, err = io.Copy(xrayFile, rc)
			if err != nil {
				return err
			}
			break
		}
	}

	os.Remove(zipPath)
	log.Println("Xray is ready.")
	return nil
}

func mustParseInt(s string) int {
	i, _ := strconv.Atoi(s)
	return i
}
