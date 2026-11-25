package importevents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// SanitizeContent готовит content к записи в jsonb. PostgreSQL не принимает в
// jsonb символ \u0000 и одиночные суррогаты, а Synapse хранит JSON текстом и
// такие события у него есть: раньше одно такое сообщение роняло всю пачку.
// Символ заменяется на U+FFFD; исходный PDU сохраняется как есть.
func SanitizeContent(content []byte) ([]byte, error) {
	if !needsSanitize(content) {
		return content, nil
	}

	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber() // числа не должны превратиться в float64

	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}

	var out bytes.Buffer

	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(cleanValue(value)); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}

	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// needsSanitize - в тексте есть escape \u0000 или суррогат \uD800-\uDFFF.
// Ложные срабатывания (экранированный обратный слеш) безвредны: значение
// просто перекодируется без изменений.
func needsSanitize(content []byte) bool {
	for i := 0; i+5 < len(content); i++ {
		if content[i] != '\\' || content[i+1] != 'u' {
			continue
		}

		hex := strings.ToLower(string(content[i+2 : i+6]))
		if hex == "0000" || (hex[0] == 'd' && hex[1] >= '8') {
			return true
		}
	}

	return false
}

func cleanValue(value any) any {
	switch v := value.(type) {
	case string:
		return strings.ReplaceAll(v, "\x00", "�")
	case []any:
		for i := range v {
			v[i] = cleanValue(v[i])
		}

		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[strings.ReplaceAll(key, "\x00", "�")] = cleanValue(item)
		}

		return out
	default:
		return v
	}
}
