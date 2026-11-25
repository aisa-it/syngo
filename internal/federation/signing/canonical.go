package signing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// ErrNotCanonical - значение нельзя представить каноническим JSON Matrix:
// дробное число, целое вне [-(2^53)+1, (2^53)-1], битый UTF-8 или одиночный
// суррогат в строке (спецификация, приложение «Canonical JSON»).
var ErrNotCanonical = errors.New("signing: value is not representable as canonical JSON")

// maxSafeInteger - 2^53-1, граница целых канонического JSON.
const maxSafeInteger = 1<<53 - 1

// CanonicalJSON приводит значение к каноническому виду Matrix (спецификация,
// приложение «Canonical JSON»): ключи объектов отсортированы по кодовым точкам,
// нет пробелов, строки в UTF-8 без лишних экранирований (экранируются только
// ", \ и управляющие символы U+0000-U+001F), числа - целые без экспоненты и
// дробной части в диапазоне ±(2^53-1). Используется для подписей (своих и
// проверки чужих), content hash и reference hash.
//
// Раньше канонизация шла через encoding/json: U+2028/U+2029 экранировались,
// дробные числа и целые вне ±2^53 переписывались, битый UTF-8 молча заменялся
// на U+FFFD - подпись и event_id расходились с Synapse. Теперь
// такие значения - ошибка ErrNotCanonical.
func CanonicalJSON(v any) ([]byte, error) {
	// Строки Go с битым UTF-8 encoding/json молча заменяет на U+FFFD - проверяем
	// их до кодирования.
	if err := checkUTF8(v); err != nil {
		return nil, err
	}

	// json.Encoder с SetEscapeHTML(false): иначе Go экранирует &, < и > в &/</>,
	// а Synapse в каноническом JSON оставляет их как есть. Экранирования
	// Go (  и т.п.) дальше раскрывает разбор canonicalize.
	var rawBuf bytes.Buffer

	encoder := json.NewEncoder(&rawBuf)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(v); err != nil {
		return nil, fmt.Errorf("signing: marshal canonical: %w", err)
	}

	return Canonicalize(bytes.TrimRight(rawBuf.Bytes(), "\n"))
}

// canonicalJSONSynapse - канонизация входа подписи X-Matrix так, как ее делает
// Synapse (python canonicaljson: json.dumps с sort_keys и ensure_ascii=False).
// Тело запроса - произвольный JSON, ограничения на числа спецификация задает только
// для событий: большие целые остаются как есть, дробные пишутся как repr float
// Python (3.5, 100.0, 1e+16). Строки - как в CanonicalJSON.
func canonicalJSONSynapse(v any) ([]byte, error) {
	if err := checkUTF8(v); err != nil {
		return nil, err
	}

	var rawBuf bytes.Buffer

	encoder := json.NewEncoder(&rawBuf)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(v); err != nil {
		return nil, fmt.Errorf("signing: marshal canonical: %w", err)
	}

	return canonicalize(bytes.TrimRight(rawBuf.Bytes(), "\n"), false)
}

// Canonicalize переводит JSON-текст в канонический JSON Matrix. В отличие от
// CanonicalJSON вход не проходит через encoding/json: битый UTF-8 и одиночные
// суррогаты (\ud800) - ошибка, а не U+FFFD.
func Canonicalize(raw []byte) ([]byte, error) {
	return canonicalize(raw, true)
}

// canonicalize - разбор с записью канонической формы; strict - правила чисел
// спецификации (только целые ±(2^53-1)), иначе - как python canonicaljson.
func canonicalize(raw []byte, strict bool) ([]byte, error) {
	parser := canonicalParser{data: raw, strict: strict}

	var out bytes.Buffer

	parser.skipSpace()

	if err := parser.value(&out); err != nil {
		return nil, err
	}

	parser.skipSpace()

	if parser.pos != len(parser.data) {
		return nil, fmt.Errorf("%w: trailing data at offset %d", ErrNotCanonical, parser.pos)
	}

	return out.Bytes(), nil
}

// checkUTF8 проверяет строки в типичных для событий значениях Go (map/slice/string).
// json.RawMessage проверяет сам разбор Canonicalize.
func checkUTF8(v any) error {
	switch value := v.(type) {
	case string:
		if !utf8.ValidString(value) {
			return fmt.Errorf("%w: invalid UTF-8 in string", ErrNotCanonical)
		}
	case map[string]any:
		for key, item := range value {
			if err := checkUTF8(key); err != nil {
				return err
			}

			if err := checkUTF8(item); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range value {
			if err := checkUTF8(item); err != nil {
				return err
			}
		}
	case map[string]string:
		for key, item := range value {
			if !utf8.ValidString(key) || !utf8.ValidString(item) {
				return fmt.Errorf("%w: invalid UTF-8 in string", ErrNotCanonical)
			}
		}
	case []string:
		for _, item := range value {
			if !utf8.ValidString(item) {
				return fmt.Errorf("%w: invalid UTF-8 in string", ErrNotCanonical)
			}
		}
	}

	return nil
}

// canonicalParser - разбор JSON (RFC 8259) с одновременной записью канонической формы.
type canonicalParser struct {
	data   []byte
	pos    int
	strict bool
}

// maxCanonicalDepth - предел вложенности: защита стека от злонамеренного PDU.
const maxCanonicalDepth = 512

func (p *canonicalParser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *canonicalParser) errorf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format+" at offset %d", append([]any{ErrNotCanonical}, append(args, p.pos)...)...)
}

func (p *canonicalParser) value(out *bytes.Buffer) error {
	return p.valueDepth(out, 0)
}

func (p *canonicalParser) valueDepth(out *bytes.Buffer, depth int) error {
	if depth > maxCanonicalDepth {
		return p.errorf("nesting too deep")
	}

	if p.pos >= len(p.data) {
		return p.errorf("unexpected end of input")
	}

	switch c := p.data[p.pos]; {
	case c == '{':
		return p.object(out, depth)
	case c == '[':
		return p.array(out, depth)
	case c == '"':
		s, err := p.str()
		if err != nil {
			return err
		}

		writeCanonicalString(out, s)

		return nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number(out)
	default:
		for _, literal := range []string{"true", "false", "null"} {
			if bytes.HasPrefix(p.data[p.pos:], []byte(literal)) {
				p.pos += len(literal)
				out.WriteString(literal)

				return nil
			}
		}

		return p.errorf("unexpected character %q", c)
	}
}

func (p *canonicalParser) object(out *bytes.Buffer, depth int) error {
	p.pos++ // '{'

	type member struct {
		key   string
		value []byte
	}

	// Повтор ключа: побеждает последний (как json.loads Synapse и encoding/json).
	members := map[string][]byte{}

	p.skipSpace()

	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		out.WriteString("{}")

		return nil
	}

	for {
		p.skipSpace()

		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return p.errorf("expected object key")
		}

		key, err := p.str()
		if err != nil {
			return err
		}

		p.skipSpace()

		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return p.errorf("expected ':'")
		}

		p.pos++
		p.skipSpace()

		var value bytes.Buffer
		if err := p.valueDepth(&value, depth+1); err != nil {
			return err
		}

		members[key] = value.Bytes()

		p.skipSpace()

		if p.pos >= len(p.data) {
			return p.errorf("unterminated object")
		}

		if p.data[p.pos] == ',' {
			p.pos++

			continue
		}

		if p.data[p.pos] == '}' {
			p.pos++

			break
		}

		return p.errorf("expected ',' or '}'")
	}

	// Сортировка по байтам UTF-8 совпадает с сортировкой по кодовым точкам.
	sorted := make([]member, 0, len(members))
	for key, value := range members {
		sorted = append(sorted, member{key: key, value: value})
	}

	sort.Slice(sorted, func(i, j int) bool { return sorted[i].key < sorted[j].key })

	out.WriteByte('{')

	for i := range sorted {
		if i > 0 {
			out.WriteByte(',')
		}

		writeCanonicalString(out, sorted[i].key)
		out.WriteByte(':')
		out.Write(sorted[i].value)
	}

	out.WriteByte('}')

	return nil
}

func (p *canonicalParser) array(out *bytes.Buffer, depth int) error {
	p.pos++ // '['

	out.WriteByte('[')

	p.skipSpace()

	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		out.WriteByte(']')

		return nil
	}

	for first := true; ; first = false {
		if !first {
			out.WriteByte(',')
		}

		p.skipSpace()

		if err := p.valueDepth(out, depth+1); err != nil {
			return err
		}

		p.skipSpace()

		if p.pos >= len(p.data) {
			return p.errorf("unterminated array")
		}

		if p.data[p.pos] == ',' {
			p.pos++

			continue
		}

		if p.data[p.pos] == ']' {
			p.pos++
			out.WriteByte(']')

			return nil
		}

		return p.errorf("expected ',' or ']'")
	}
}

// number пишет число каноническим целым. Запись с экспонентой или нулевой дробной
// частью, равная целому из диапазона, приводится к целому (вектор спецификации:
// -0 -> 0, 1e10 -> 10000000000); дробные и большие числа - ошибка.
func (p *canonicalParser) number(out *bytes.Buffer) error {
	start := p.pos

	if p.data[p.pos] == '-' {
		p.pos++
	}

	digits := p.pos
	for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
		p.pos++
	}

	if p.pos == digits {
		return p.errorf("invalid number")
	}

	if p.data[digits] == '0' && p.pos-digits > 1 {
		return p.errorf("leading zero in number")
	}

	simple := true

	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		simple = false
		p.pos++

		fraction := p.pos
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}

		if p.pos == fraction {
			return p.errorf("invalid number")
		}
	}

	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		simple = false
		p.pos++

		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}

		exponent := p.pos
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}

		if p.pos == exponent {
			return p.errorf("invalid number")
		}
	}

	literal := string(p.data[start:p.pos])

	if !p.strict {
		return writePythonNumber(out, literal, simple)
	}

	if simple && len(literal) <= 17 {
		n, err := strconv.ParseInt(literal, 10, 64)
		if err == nil && n >= -maxSafeInteger && n <= maxSafeInteger {
			out.WriteString(strconv.FormatInt(n, 10))

			return nil
		}

		return fmt.Errorf("%w: integer %s out of range", ErrNotCanonical, literal)
	}

	// Экспонента огромной величины (1e999999999) не разворачивается в big.Rat:
	// такие значения заведомо вне диапазона.
	if len(literal) > 64 {
		return fmt.Errorf("%w: number %.32s... out of range", ErrNotCanonical, literal)
	}

	rat, ok := new(big.Rat).SetString(literal)
	if !ok {
		return p.errorf("invalid number %s", literal)
	}

	if !rat.IsInt() {
		return fmt.Errorf("%w: float %s is not allowed", ErrNotCanonical, literal)
	}

	n := rat.Num()
	if !n.IsInt64() || n.Int64() < -maxSafeInteger || n.Int64() > maxSafeInteger {
		return fmt.Errorf("%w: integer %s out of range", ErrNotCanonical, literal)
	}

	out.WriteString(strconv.FormatInt(n.Int64(), 10))

	return nil
}

// writePythonNumber пишет число так, как его выводит json.dumps Python после
// json.loads: целое - точными цифрами (-0 -> 0), дробное - repr float.
func writePythonNumber(out *bytes.Buffer, literal string, integer bool) error {
	if integer {
		n, ok := new(big.Int).SetString(literal, 10)
		if !ok {
			return fmt.Errorf("%w: invalid number %s", ErrNotCanonical, literal)
		}

		out.WriteString(n.String())

		return nil
	}

	f, err := strconv.ParseFloat(literal, 64)
	if err != nil || math.IsInf(f, 0) {
		return fmt.Errorf("%w: number %.32s is not a finite float", ErrNotCanonical, literal)
	}

	out.WriteString(pythonFloatRepr(f))

	return nil
}

// pythonFloatRepr - repr(float) Python: кратчайшие цифры, фиксированная запись при
// показателе в [-4, 16), иначе экспонента с двумя цифрами минимум (1e+16, 1e-05).
func pythonFloatRepr(f float64) string {
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}

		return "0.0"
	}

	sign := ""
	if f < 0 {
		sign = "-"
		f = -f
	}

	// Кратчайшее представление: d.ddde±XX.
	formatted := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, expText, _ := strings.Cut(formatted, "e")
	digits := strings.Replace(mantissa, ".", "", 1)

	exp, _ := strconv.Atoi(expText)

	if exp < -4 || exp >= 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}

		expSign := "+"
		if exp < 0 {
			expSign = "-"
			exp = -exp
		}

		return fmt.Sprintf("%s%se%s%02d", sign, m, expSign, exp)
	}

	if exp < 0 {
		return sign + "0." + strings.Repeat("0", -exp-1) + digits
	}

	if len(digits) <= exp+1 {
		return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	}

	return sign + digits[:exp+1] + "." + digits[exp+1:]
}

// str разбирает строку JSON: экранирования раскрываются, суррогатные пары
// склеиваются, одиночный суррогат и битый UTF-8 - ошибка.
func (p *canonicalParser) str() (string, error) {
	p.pos++ // '"'

	var buf []byte

	for {
		if p.pos >= len(p.data) {
			return "", p.errorf("unterminated string")
		}

		c := p.data[p.pos]

		switch {
		case c == '"':
			p.pos++

			return string(buf), nil
		case c == '\\':
			r, err := p.escape()
			if err != nil {
				return "", err
			}

			buf = utf8.AppendRune(buf, r)
		case c < 0x20:
			return "", p.errorf("control character in string")
		case c < utf8.RuneSelf:
			buf = append(buf, c)
			p.pos++
		default:
			r, size := utf8.DecodeRune(p.data[p.pos:])
			if r == utf8.RuneError && size <= 1 {
				return "", p.errorf("invalid UTF-8 in string")
			}

			buf = append(buf, p.data[p.pos:p.pos+size]...)
			p.pos += size
		}
	}
}

// escape разбирает экранирование после '\'.
func (p *canonicalParser) escape() (rune, error) {
	if p.pos+1 >= len(p.data) {
		return 0, p.errorf("unterminated escape")
	}

	c := p.data[p.pos+1]
	p.pos += 2

	switch c {
	case '"', '\\', '/':
		return rune(c), nil
	case 'b':
		return '\b', nil
	case 'f':
		return '\f', nil
	case 'n':
		return '\n', nil
	case 'r':
		return '\r', nil
	case 't':
		return '\t', nil
	case 'u':
		r, err := p.hex4()
		if err != nil {
			return 0, err
		}

		if !utf16.IsSurrogate(r) {
			return r, nil
		}

		// Старший суррогат обязан идти в паре с младшим: 😀.
		if r < 0xDC00 && p.pos+1 < len(p.data) && p.data[p.pos] == '\\' && p.data[p.pos+1] == 'u' {
			p.pos += 2

			low, err := p.hex4()
			if err != nil {
				return 0, err
			}

			if combined := utf16.DecodeRune(r, low); combined != utf8.RuneError {
				return combined, nil
			}
		}

		return 0, p.errorf("lone surrogate in string")
	default:
		return 0, p.errorf("invalid escape \\%c", c)
	}
}

func (p *canonicalParser) hex4() (rune, error) {
	if p.pos+4 > len(p.data) {
		return 0, p.errorf("short \\u escape")
	}

	var value rune

	for _, c := range p.data[p.pos : p.pos+4] {
		var digit byte

		switch {
		case c >= '0' && c <= '9':
			digit = c - '0'
		case c >= 'a' && c <= 'f':
			digit = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			digit = c - 'A' + 10
		default:
			return 0, p.errorf("invalid \\u escape")
		}

		value = value<<4 | rune(digit)
	}

	p.pos += 4

	return value, nil
}

// writeCanonicalString пишет строку по грамматике спецификации: ", \ и управляющие
// символы экранируются (\b \t \n \f \r или \u00XX строчными), все остальное, включая
// U+2028/U+2029, DEL и не-ASCII, - как есть в UTF-8.
func writeCanonicalString(out *bytes.Buffer, s string) {
	const hexDigits = "0123456789abcdef"

	out.WriteByte('"')

	for i := 0; i < len(s); i++ {
		c := s[i]

		switch c {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if c < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hexDigits[c>>4])
				out.WriteByte(hexDigits[c&0xF])

				continue
			}

			out.WriteByte(c)
		}
	}

	out.WriteByte('"')
}

// jsonAny разбирает сырой JSON в значение с json.Number (для канонизации).
func jsonAny(body []byte) any {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}

	return v
}
