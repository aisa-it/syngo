// Package mapping переводит claims провайдера OIDC в localpart и displayname, как
// user_mapping_provider Synapse (JinjaOidcMappingProvider).
//
// Шаблоны - подмножество Jinja2, которым пользуются конфиги Synapse:
// `{{ user.<claim> }}` (вложенные claims через точку) и фильтры `lower`, `upper`,
// `trim`, `localpart_from_email`. Остальной синтаксис Jinja отклоняется при разборе
// конфига, а не молча рендерится в пустую строку.
package mapping

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrTemplate - шаблон не разбирается.
var ErrTemplate = errors.New("mapping: invalid template")

// claimPath - user.<имя>(.<имя>)*: имена claims из латиницы, цифр, _ и -.
var claimPath = regexp.MustCompile(`^user(\.[A-Za-z0-9_-]+)+$`)

// filters - поддержанные фильтры Jinja.
var filters = map[string]func(string) string{
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"trim":  strings.TrimSpace,
	// localpart_from_email - часть адреса до "@" (фильтр Synapse, synapse/util/templates.py).
	"localpart_from_email": func(value string) string {
		local, _, _ := strings.Cut(value, "@")

		return local
	},
}

// Template - разобранный шаблон.
type Template struct {
	parts []part
}

// part - литерал или подстановка claim с фильтрами.
type part struct {
	literal string
	path    []string
	filters []string
}

// Parse разбирает шаблон. Пустая строка - пустой шаблон (Render вернет "").
func Parse(source string) (*Template, error) {
	tmpl := &Template{}
	rest := source

	for rest != "" {
		start := strings.Index(rest, "{{")
		if start < 0 {
			if strings.Contains(rest, "}}") || strings.Contains(rest, "{%") {
				return nil, fmt.Errorf("%w: %q", ErrTemplate, source)
			}

			tmpl.parts = append(tmpl.parts, part{literal: rest})

			break
		}

		if literal := rest[:start]; literal != "" {
			if strings.Contains(literal, "}}") || strings.Contains(literal, "{%") {
				return nil, fmt.Errorf("%w: %q", ErrTemplate, source)
			}

			tmpl.parts = append(tmpl.parts, part{literal: literal})
		}

		end := strings.Index(rest[start:], "}}")
		if end < 0 {
			return nil, fmt.Errorf("%w: незакрытое {{ в %q", ErrTemplate, source)
		}

		expr, err := parseExpr(rest[start+2 : start+end])
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %w", ErrTemplate, source, err)
		}

		tmpl.parts = append(tmpl.parts, expr)
		rest = rest[start+end+2:]
	}

	return tmpl, nil
}

// parseExpr разбирает выражение внутри {{ }}: путь claim и фильтры через |.
func parseExpr(source string) (part, error) {
	pieces := strings.Split(source, "|")
	path := strings.TrimSpace(pieces[0])

	if !claimPath.MatchString(path) {
		return part{}, fmt.Errorf("ожидается user.<claim>, получено %q", path)
	}

	expr := part{path: strings.Split(path, ".")[1:]}

	for _, raw := range pieces[1:] {
		name := strings.TrimSpace(raw)
		if _, ok := filters[name]; !ok {
			return part{}, fmt.Errorf("фильтр %q не поддержан (lower, upper, trim, localpart_from_email)", name)
		}

		expr.filters = append(expr.filters, name)
	}

	return expr, nil
}

// Empty - шаблон пустой (не задан).
func (t *Template) Empty() bool {
	return t == nil || len(t.parts) == 0
}

// Render подставляет claims. Отсутствующий claim - пустая строка, как undefined в Jinja.
func (t *Template) Render(claims map[string]any) string {
	if t == nil {
		return ""
	}

	var out strings.Builder

	for _, p := range t.parts {
		if p.path == nil {
			out.WriteString(p.literal)

			continue
		}

		value := ClaimString(lookup(claims, p.path))
		for _, name := range p.filters {
			value = filters[name](value)
		}

		out.WriteString(value)
	}

	return out.String()
}

// lookup достает вложенный claim по пути.
func lookup(claims map[string]any, path []string) any {
	var current any = claims

	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}

		current = object[key]
	}

	return current
}

// ClaimString - строковое представление claim: строка как есть, число без
// экспоненты (sub бывает числом), bool - true/false, остальное - пусто.
func ClaimString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case interface{ String() string }:
		return v.String()
	default:
		return ""
	}
}

// Claim достает claim верхнего уровня или вложенный (через точку) строкой.
func Claim(claims map[string]any, name string) string {
	return ClaimString(lookup(claims, strings.Split(name, ".")))
}

// mxidAllowed - символы localpart, которые map_username_to_mxid_localpart Synapse
// оставляет как есть ("=" экранируется).
func mxidAllowed(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || strings.IndexByte("_-./+", b) >= 0
}

// MXIDLocalpart повторяет map_username_to_mxid_localpart Synapse (synapse/types):
// нижний регистр только для ASCII (bytes.lower в Python), остальные недопустимые
// байты UTF-8 - "=xx", ведущий "_" - "=5f". Так из claims получается тот же
// localpart, что выдал бы Synapse.
func MXIDLocalpart(username string) string {
	var out strings.Builder

	for _, b := range []byte(username) {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}

		if mxidAllowed(b) {
			out.WriteByte(b)

			continue
		}

		fmt.Fprintf(&out, "=%02x", b)
	}

	result := out.String()
	if strings.HasPrefix(result, "_") {
		result = "=5f" + result[1:]
	}

	return result
}
