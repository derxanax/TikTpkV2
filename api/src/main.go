package main

import (
	"fmt"      // ! форматирование
	"io"       // ! чтение
	"net/http" // ! запросы
	"strings"  // ! работа со строками
)

// * качает список урлов из текстового файла
func GetProxyURLs(url string) ([]string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("не смог скачать подписку: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("сервер ответил криво: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("не смог прочитать ответ: %w", err)
	}

	lines := strings.Split(string(body), "\n")
	var proxies []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			proxies = append(proxies, trimmed)
		}
	}

	if len(proxies) == 0 {
		return nil, fmt.Errorf("в подписке пусто крч")
	}

	return proxies, nil
}