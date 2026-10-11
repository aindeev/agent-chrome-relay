package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// object is a JSON object that keeps its keys in file order, like Python's dicts: identities.json and config.json
// are edited by hand, and the CLI prints them in the order the person wrote them.
type object struct {
	keys []string
	vals map[string]any // *object, []any, string, json.Number, bool or nil
}

func newObject() *object { return &object{vals: map[string]any{}} }

func (o *object) get(k string) any { return o.vals[k] }

func (o *object) has(k string) bool {
	_, ok := o.vals[k]
	return ok
}

// set keeps an existing key where it is; a new key goes last.
func (o *object) set(k string, v any) {
	if !o.has(k) {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) del(k string) {
	if !o.has(k) {
		return
	}
	delete(o.vals, k)
	for i, x := range o.keys {
		if x == k {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
}

// obj returns the object at k, or an empty one (not stored) when missing or not an object.
func (o *object) obj(k string) *object {
	if x, ok := o.vals[k].(*object); ok {
		return x
	}
	return newObject()
}

func (o *object) str(k string) string {
	s, _ := o.vals[k].(string)
	return s
}

func (o *object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// marshal without HTML escaping: these files are read by people and by the relay, never by a browser.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// parseJSON decodes any JSON value, objects as *object (in order), numbers as json.Number.
func parseJSON(data []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := decodeValue(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err == nil {
		return nil, fmt.Errorf("extra data after the JSON value")
	}
	return v, nil
}

func decodeValue(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		o := newObject()
		for d.More() {
			kt, err := d.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeValue(d)
			if err != nil {
				return nil, err
			}
			o.set(kt.(string), v)
		}
		_, err := d.Token()
		return o, err
	case json.Delim('['):
		a := []any{}
		for d.More() {
			v, err := decodeValue(d)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err := d.Token()
		return a, err
	}
	return t, nil
}

// writeJSON writes v indented by 2, as Python's json.dump(indent=2) did; atomically, since the relay reads
// these files while it runs.
func writeJSON(path string, v any) error {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(v); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bytes.TrimRight(b.Bytes(), "\n")); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil { // private: which accounts and sites this Mac uses
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// --- printing JSON values the way the Python CLI did (str(), repr(), format widths) ---

// pyStr is Python's str() of a decoded JSON value.
func pyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyRepr(v)
}

// pyRepr is Python's repr() of a decoded JSON value.
func pyRepr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case string:
		return pyQuote(x)
	case json.Number:
		return pyNumber(x)
	case []string:
		parts := make([]string, len(x))
		for i, s := range x {
			parts[i] = pyQuote(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = pyRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *object:
		parts := make([]string, len(x.keys))
		for i, k := range x.keys {
			parts[i] = pyQuote(k) + ": " + pyRepr(x.vals[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

func pyQuote(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == '\\' || r == rune(q):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f || (r > 0x7f && r <= 0xff && !unicode.IsPrint(r)):
			fmt.Fprintf(&b, `\x%02x`, r)
		case r > 0xff && !unicode.IsPrint(r):
			if r <= 0xffff {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}

// pyNumber: JSON integers stay integers; others print like Python's float repr (1.0, 1.5e-05, 1e+16).
func pyNumber(n json.Number) string {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if i, ok := new(big.Int).SetString(s, 10); ok {
			return i.String()
		}
		return s
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !math.IsInf(f, 0) {
		return s
	}
	return pyFloat(f)
}

func pyFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case f == 0:
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // shortest round-trip digits, like Python
	mant, expStr, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.Replace(mant, ".", "", 1)
	if -4 <= exp && exp < 16 {
		var whole, frac string
		if exp >= 0 {
			if len(digits) <= exp+1 {
				whole, frac = digits+strings.Repeat("0", exp+1-len(digits)), "0"
			} else {
				whole, frac = digits[:exp+1], digits[exp+1:]
			}
		} else {
			whole, frac = "0", strings.Repeat("0", -exp-1)+digits
		}
		return sign + whole + "." + frac
	}
	m := digits[:1]
	if len(digits) > 1 {
		m += "." + digits[1:]
	}
	es := "+"
	if exp < 0 {
		es, exp = "-", -exp
	}
	return fmt.Sprintf("%s%se%s%02d", sign, m, es, exp)
}

// pyFormat is Python's f"{v:width}": text left-aligned, numbers right-aligned (bools as 1/0, as Python does).
func pyFormat(v any, width int) string {
	switch x := v.(type) {
	case json.Number:
		return fmt.Sprintf("%*s", width, pyNumber(x))
	case bool:
		n := "0"
		if x {
			n = "1"
		}
		return fmt.Sprintf("%*s", width, n)
	}
	return fmt.Sprintf("%-*s", width, pyStr(v))
}

// cut is Python's s[:n]: the first n characters.
func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
