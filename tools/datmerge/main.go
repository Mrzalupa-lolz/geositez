// datmerge склеивает несколько geosite .dat в один и вычитает пересечения
// между категориями.
//
// Зачем. Клиенты вроде INCY принимают РОВНО ОДИН geosite-файл (поле
// Geositeurl) и раскладывают правила не по порядку, а по плоским корзинам
// DirectSites / ProxySites / BlockSites. Порядок правил там задать нельзя,
// поэтому домен, попавший сразу в две категории из разных корзин, ведёт себя
// непредсказуемо — как решит приоритет внутри клиента.
//
// Лечится на этапе сборки: делаем категории непересекающимися.
//
//	go run ./tools/datmerge -out dist/client-geosite.dat \
//	    -sub TWITCH-TWITCH-ADS -sub APPLE-CONNECTIVITY -sub GOOGLE-PLAY-SYSTEM \
//	    dist/custom-geosite.dat rv-geosite.dat
//
// Первый файл в списке приоритетнее: если код категории встречается дважды,
// берётся версия из более раннего файла.
//
// -sub A-B читается как «выкинуть из A всё, что и так ловится категорией B».
// Вычитание учитывает не только точные совпадения, но и перекрытия: поддомен
// под domain:, full: под domain:, что угодно под keyword:.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

const (
	typeSubstr uint64 = 0 // keyword
	typeRegex  uint64 = 1
	typeDomain uint64 = 2
	typeFull   uint64 = 3
)

type domainEntry struct {
	typ   uint64
	value string
	attrs []string
}

type category struct {
	code    string
	domains []domainEntry
	src     string
}

type subList []string

func (s *subList) String() string     { return strings.Join(*s, ",") }
func (s *subList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	out := flag.String("out", "dist/client-geosite.dat", "куда положить склеенный файл")
	var subs subList
	flag.Var(&subs, "sub", "вычитание вида A-B: убрать из категории A всё, что ловит B (можно несколько раз)")
	flag.Parse()

	if flag.NArg() < 1 {
		fatal(fmt.Errorf("не заданы входные .dat — укажи их аргументами"))
	}

	var cats []*category
	index := map[string]*category{}
	for _, path := range flag.Args() {
		b, err := os.ReadFile(path)
		fatal(err)
		list, err := decode(b)
		fatal(err)
		for _, c := range list {
			c.src = path
			if prev, ok := index[c.code]; ok {
				fmt.Fprintf(os.Stderr, "warning: категория %s есть и в %s (%d записей), и в %s (%d) — беру первую\n",
					c.code, prev.src, len(prev.domains), path, len(c.domains))
				continue
			}
			index[c.code] = c
			cats = append(cats, c)
		}
		fmt.Fprintf(os.Stderr, "%s: %d категорий\n", path, len(list))
	}

	for _, s := range subs {
		from, minus, err := splitSub(s, index)
		fatal(err)
		before := len(index[from].domains)
		index[from].domains = subtract(index[from].domains, index[minus].domains)
		fmt.Fprintf(os.Stderr, "вычитание %s -= %s: %d -> %d записей\n",
			from, minus, before, len(index[from].domains))
	}

	var blob []byte
	total := 0
	for _, c := range cats {
		if len(c.domains) == 0 {
			fmt.Fprintf(os.Stderr, "warning: категория %s опустела, пропускаю\n", c.code)
			continue
		}
		blob = appendBytesField(blob, 1, encodeGeoSite(c.code, c.domains))
		total += len(c.domains)
	}
	fatal(os.WriteFile(*out, blob, 0o644))

	codes := make([]string, 0, len(cats))
	for _, c := range cats {
		if len(c.domains) > 0 {
			codes = append(codes, c.code)
		}
	}
	sort.Strings(codes)
	fmt.Printf("%s: %d категорий, %d записей, %d байт\n", *out, len(codes), total, len(blob))
	fmt.Println("категории: " + strings.Join(codes, ", "))
}

// splitSub разбирает "A-B". Коды сами содержат дефисы (TWITCH-ADS,
// GOOGLE-PLAY), поэтому перебираем все точки разреза и берём ту, где обе
// половины — существующие категории.
func splitSub(s string, index map[string]*category) (string, string, error) {
	up := strings.ToUpper(s)
	for i := 1; i < len(up); i++ {
		if up[i] != '-' {
			continue
		}
		a, b := up[:i], up[i+1:]
		if index[a] != nil && index[b] != nil {
			return a, b, nil
		}
	}
	return "", "", fmt.Errorf("-sub %s: не нашёл пары существующих категорий (проверь коды)", s)
}

// subtract убирает из a всё, что уже ловится правилами из b.
func subtract(a, b []domainEntry) []domainEntry {
	exact := map[string]bool{}
	suffix := map[string]bool{}
	var keywords []string
	for _, e := range b {
		switch e.typ {
		case typeDomain:
			suffix[e.value] = true
			exact[key(e)] = true
		case typeSubstr:
			keywords = append(keywords, e.value)
		default:
			exact[key(e)] = true
		}
	}

	covered := func(e domainEntry) bool {
		if exact[key(e)] {
			return true
		}
		if e.typ == typeDomain || e.typ == typeFull {
			for s := e.value; ; {
				if suffix[s] {
					return true
				}
				i := strings.Index(s, ".")
				if i < 0 {
					break
				}
				s = s[i+1:]
			}
		}
		if e.typ != typeRegex {
			for _, k := range keywords {
				if strings.Contains(e.value, k) {
					return true
				}
			}
		}
		return false
	}

	out := a[:0:0]
	for _, e := range a {
		if !covered(e) {
			out = append(out, e)
		}
	}
	return out
}

func key(e domainEntry) string { return fmt.Sprintf("%d\x00%s", e.typ, e.value) }

// ---------- разбор .dat ----------

func decode(b []byte) ([]*category, error) {
	var out []*category
	i := 0
	for i < len(b) {
		tag, n, err := varint(b, i)
		if err != nil {
			return nil, err
		}
		i = n
		if tag != 1<<3|2 {
			return nil, fmt.Errorf("неожиданное поле %d в GeoSiteList", tag>>3)
		}
		ln, n, err := varint(b, i)
		if err != nil {
			return nil, err
		}
		i = n
		if i+int(ln) > len(b) {
			return nil, fmt.Errorf("файл обрывается на середине категории")
		}
		c, err := decodeGeoSite(b[i : i+int(ln)])
		if err != nil {
			return nil, err
		}
		i += int(ln)
		out = append(out, c)
	}
	return out, nil
}

func decodeGeoSite(b []byte) (*category, error) {
	c := &category{}
	i := 0
	for i < len(b) {
		tag, n, err := varint(b, i)
		if err != nil {
			return nil, err
		}
		i = n
		ln, n, err := varint(b, i)
		if err != nil {
			return nil, err
		}
		i = n
		if i+int(ln) > len(b) {
			return nil, fmt.Errorf("категория обрывается")
		}
		payload := b[i : i+int(ln)]
		i += int(ln)
		switch tag >> 3 {
		case 1:
			c.code = string(payload)
		case 2:
			d, err := decodeDomain(payload)
			if err != nil {
				return nil, err
			}
			c.domains = append(c.domains, d)
		}
	}
	if c.code == "" {
		return nil, fmt.Errorf("у категории нет кода")
	}
	return c, nil
}

func decodeDomain(b []byte) (domainEntry, error) {
	var d domainEntry
	i := 0
	for i < len(b) {
		tag, n, err := varint(b, i)
		if err != nil {
			return d, err
		}
		i = n
		switch {
		case tag == 1<<3|0: // type, varint
			v, n, err := varint(b, i)
			if err != nil {
				return d, err
			}
			d.typ, i = v, n
		case tag == 2<<3|2: // value, bytes
			ln, n, err := varint(b, i)
			if err != nil {
				return d, err
			}
			i = n
			d.value = string(b[i : i+int(ln)])
			i += int(ln)
		case tag == 3<<3|2: // attribute, bytes
			ln, n, err := varint(b, i)
			if err != nil {
				return d, err
			}
			i = n
			a, err := decodeAttr(b[i : i+int(ln)])
			if err != nil {
				return d, err
			}
			if a != "" {
				d.attrs = append(d.attrs, a)
			}
			i += int(ln)
		default:
			return d, fmt.Errorf("неизвестное поле %d в Domain", tag>>3)
		}
	}
	return d, nil
}

func decodeAttr(b []byte) (string, error) {
	i := 0
	key := ""
	for i < len(b) {
		tag, n, err := varint(b, i)
		if err != nil {
			return "", err
		}
		i = n
		if tag == 1<<3|2 {
			ln, n, err := varint(b, i)
			if err != nil {
				return "", err
			}
			i = n
			key = string(b[i : i+int(ln)])
			i += int(ln)
			continue
		}
		if _, n, err := varint(b, i); err == nil { // bool_value
			i = n
			continue
		}
		break
	}
	return key, nil
}

func varint(b []byte, i int) (uint64, int, error) {
	var v uint64
	var s uint
	for {
		if i >= len(b) {
			return 0, 0, fmt.Errorf("varint обрывается")
		}
		x := b[i]
		i++
		v |= uint64(x&0x7f) << s
		if x&0x80 == 0 {
			return v, i, nil
		}
		s += 7
		if s > 63 {
			return 0, 0, fmt.Errorf("varint слишком длинный")
		}
	}
}

// ---------- сборка .dat (как в geogen) ----------

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendTag(b []byte, field, wire int) []byte {
	return appendVarint(b, uint64(field)<<3|uint64(wire))
}

func appendBytesField(b []byte, field int, data []byte) []byte {
	b = appendTag(b, field, 2)
	b = appendVarint(b, uint64(len(data)))
	return append(b, data...)
}

func appendVarintField(b []byte, field int, v uint64) []byte {
	b = appendTag(b, field, 0)
	return appendVarint(b, v)
}

func encodeDomain(d domainEntry) []byte {
	var b []byte
	if d.typ != 0 {
		b = appendVarintField(b, 1, d.typ)
	}
	b = appendBytesField(b, 2, []byte(d.value))
	for _, a := range d.attrs {
		var ab []byte
		ab = appendBytesField(ab, 1, []byte(a))
		ab = appendVarintField(ab, 2, 1)
		b = appendBytesField(b, 3, ab)
	}
	return b
}

// code обязан идти первым: Xray ищет запись по префиксу сообщения.
func encodeGeoSite(code string, doms []domainEntry) []byte {
	var b []byte
	b = appendBytesField(b, 1, []byte(code))
	for _, d := range doms {
		b = appendBytesField(b, 2, encodeDomain(d))
	}
	return b
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}
