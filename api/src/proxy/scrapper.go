package proxy

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
)

// GetProxyConfigs downloads a proxy subscription and returns a list of proxy URLs.
func GetProxyConfigs(url string) ([]string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Попытка декодировать из Base64. Если не вышло, считаем, что это обычный текст.
	decodedBody, err := base64.StdEncoding.DecodeString(string(body))
	var content string
	if err != nil {
		// Не удалось декодировать, значит, это обычный текст
		content = string(body)
	} else {
		content = string(decodedBody)
	}

	// Разделяем на строки и убираем пустые
	lines := strings.Split(content, "\n")
	var proxies []string
	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine != "" {
			proxies = append(proxies, trimmedLine)
		}
	}

	return proxies, nil
}
