// geogen собирает .dat файлы для Xray-core (geosite/geoip) из обычных
// текстовых списков, а заодно выгружает те же списки в форматах
// mihomo (clash) rule-provider и sing-box rule-set (source json).
//
// Зависимостей нет — только стандартная библиотека. Формат .dat —
// protobuf-сообщения GeoSiteList/GeoIPList из common/geodata/geodat.proto
// (совместим с v2ray/Xray). Кодирование написано вручную, чтобы не тащить
// protoc и внешние модули.
//
//	go run ./tools/geogen -data data -out dist
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------- модель ----------

type domainType uint64

const (
	typeSubstr domainType = 0 // keyword: подстрока
	typeRegex  domainType = 1 // regexp:
	typeDomain domainType = 2 // domain: сам домен + поддомены (дефолт)
	typeFull   domainType = 3 // full: точное совпадение
)

type domainEntry struct {
	typ   domainType
	value string
	attrs []string
}

type cidrEntry struct {
	ip     []byte
	prefix uint32
	text   string
}

type listFile struct {
	code     string   // код категории в .dat (ВЕРХНИЙ регистр)
	path     string   // путь к исходному txt
	includes []string // include: другие категории
	sites    []domainEntry
	cidrs    []cidrEntry
}

var (
	codeRe   = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]*$`)
	domainRe = regexp.MustCompile(`^[a-z0-9_*-]+(\.[a-z0-9_*-]+)*$`)
)

func main() {
	dataDir := flag.String("data", "data", "каталог с исходными списками (data/geosite, data/geoip)")
	outDir := flag.String("out", "dist", "каталог для собранных файлов")
	siteName := flag.String("site-name", "custom-geosite.dat", "имя файла geosite")
	ipName := flag.String("ip-name", "custom-geoip.dat", "имя файла geoip")
	flag.Parse()

	sites, err := loadDir(filepath.Join(*dataDir, "geosite"), false)
	fatal(err)
	ips, err := loadDir(filepath.Join(*dataDir, "geoip"), true)
	fatal(err)

	if len(sites) == 0 && len(ips) == 0 {
		fatal(fmt.Errorf("не найдено ни одного списка в %s — положи .txt в data/geosite или data/geoip", *dataDir))
	}

	fatal(os.MkdirAll(*outDir, 0o755))
	fatal(os.MkdirAll(filepath.Join(*outDir, "clash"), 0o755))
	fatal(os.MkdirAll(filepath.Join(*outDir, "singbox"), 0o755))

	var produced []string
	summary := &strings.Builder{}
	summary.WriteString("| файл | категория | записей |\n|---|---|---|\n")

	// ---- geosite ----
	if len(sites) > 0 {
		var blob []byte
		for _, l := range sites {
			doms, err := resolveSites(l.code, sites, map[string]bool{})
			fatal(err)
			if len(doms) == 0 {
				fmt.Fprintf(os.Stderr, "warning: категория %s пустая, пропускаю\n", l.code)
				continue
			}
			blob = appendBytesField(blob, 1, encodeGeoSite(l.code, doms))
			fatal(writeFile(filepath.Join(*outDir, "clash", "site-"+strings.ToLower(l.code)+".txt"), []byte(clashSite(doms))))
			fatal(writeFile(filepath.Join(*outDir, "singbox", "site-"+strings.ToLower(l.code)+".json"), singboxSite(doms)))
			fmt.Fprintf(summary, "| %s | `%s` | %d |\n", *siteName, l.code, len(doms))
		}
		p := filepath.Join(*outDir, *siteName)
		fatal(writeFile(p, blob))
		produced = append(produced, p)
	}

	// ---- geoip ----
	if len(ips) > 0 {
		var blob []byte
		for _, l := range ips {
			cidrs, err := resolveIPs(l.code, ips, map[string]bool{})
			fatal(err)
			if len(cidrs) == 0 {
				fmt.Fprintf(os.Stderr, "warning: категория %s пустая, пропускаю\n", l.code)
				continue
			}
			blob = appendBytesField(blob, 1, encodeGeoIP(l.code, cidrs))
			fatal(writeFile(filepath.Join(*outDir, "clash", "ip-"+strings.ToLower(l.code)+".txt"), []byte(clashIP(cidrs))))
			fatal(writeFile(filepath.Join(*outDir, "singbox", "ip-"+strings.ToLower(l.code)+".json"), singboxIP(cidrs)))
			fmt.Fprintf(summary, "| %s | `%s` | %d |\n", *ipName, l.code, len(cidrs))
		}
		p := filepath.Join(*outDir, *ipName)
		fatal(writeFile(p, blob))
		produced = append(produced, p)
	}

	// ---- контрольные суммы ----
	sums := &strings.Builder{}
	for _, p := range produced {
		b, err := os.ReadFile(p)
		fatal(err)
		fmt.Fprintf(sums, "%x  %s\n", sha256.Sum256(b), filepath.Base(p))
	}
	fatal(writeFile(filepath.Join(*outDir, "checksums.txt"), []byte(sums.String())))
	fatal(writeFile(filepath.Join(*outDir, "summary.md"), []byte(summary.String())))

	fmt.Print(summary.String())
	fmt.Print(sums.String())
}

// ---------- чтение исходников ----------

func loadDir(dir string, isIP bool) ([]*listFile, error) {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*listFile
	seen := map[string]string{}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".txt" && ext != ".list" && ext != "" {
			continue
		}
		code := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
		if !codeRe.MatchString(code) {
			return nil, fmt.Errorf("%s: недопустимое имя файла — только латиница, цифры, дефис и подчёркивание", filepath.Join(dir, name))
		}
		if prev, ok := seen[code]; ok {
			return nil, fmt.Errorf("категория %s объявлена дважды: %s и %s", code, prev, name)
		}
		seen[code] = name
		l, err := parseFile(filepath.Join(dir, name), code, isIP)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].code < out[j].code })
	return out, nil
}

func parseFile(path, code string, isIP bool) (*listFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	l := &listFile{code: code, path: path}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	ln := 0
	for sc.Scan() {
		ln++
		line := stripComment(sc.Text())
		if line == "" {
			continue
		}
		fail := func(msg string) error { return fmt.Errorf("%s:%d: %s (строка: %q)", path, ln, msg, line) }

		if rest, ok := cutPrefix(line, "include:"); ok {
			inc := strings.ToUpper(strings.TrimSpace(rest))
			if !codeRe.MatchString(inc) {
				return nil, fail("некорректное имя в include:")
			}
			if inc == code {
				return nil, fail("список включает сам себя")
			}
			l.includes = append(l.includes, inc)
			continue
		}

		if isIP {
			c, err := parseCIDR(line)
			if err != nil {
				return nil, fail(err.Error())
			}
			l.cidrs = append(l.cidrs, c)
			continue
		}

		d, err := parseDomain(line)
		if err != nil {
			return nil, fail(err.Error())
		}
		l.sites = append(l.sites, d)
	}
	return l, sc.Err()
}

// stripComment убирает комментарии: '#' или '//' в начале строки либо после пробела.
func stripComment(s string) string {
	s = strings.TrimSpace(strings.TrimPrefix(s, "\ufeff"))
	for i := 0; i < len(s); i++ {
		atStart := i == 0
		afterSpace := i > 0 && (s[i-1] == ' ' || s[i-1] == '\t')
		if !atStart && !afterSpace {
			continue
		}
		if s[i] == '#' || (s[i] == '/' && i+1 < len(s) && s[i+1] == '/') {
			return strings.TrimSpace(s[:i])
		}
	}
	return s
}

func cutPrefix(s, p string) (string, bool) {
	if len(s) >= len(p) && strings.EqualFold(s[:len(p)], p) {
		return s[len(p):], true
	}
	return "", false
}

func parseDomain(line string) (domainEntry, error) {
	fields := strings.Fields(line)
	head := fields[0]
	var attrs []string
	for _, f := range fields[1:] {
		if !strings.HasPrefix(f, "@") {
			return domainEntry{}, fmt.Errorf("лишний текст %q — атрибуты пишутся как @имя", f)
		}
		a := strings.ToLower(strings.TrimPrefix(f, "@"))
		if a == "" {
			return domainEntry{}, fmt.Errorf("пустой атрибут")
		}
		attrs = append(attrs, a)
	}

	typ := typeDomain
	val := head
	if i := strings.Index(head, ":"); i > 0 {
		switch strings.ToLower(head[:i]) {
		case "domain":
			typ, val = typeDomain, head[i+1:]
		case "full":
			typ, val = typeFull, head[i+1:]
		case "keyword", "substr":
			typ, val = typeSubstr, head[i+1:]
		case "regexp", "regex":
			typ, val = typeRegex, head[i+1:]
		}
	}
	if val == "" {
		return domainEntry{}, fmt.Errorf("пустое значение")
	}

	if typ == typeRegex {
		if _, err := regexp.Compile(val); err != nil {
			return domainEntry{}, fmt.Errorf("некорректное регулярное выражение: %v", err)
		}
		return domainEntry{typ: typ, value: val, attrs: attrs}, nil
	}

	val = cleanHost(val)
	if val == "" {
		return domainEntry{}, fmt.Errorf("пустое значение")
	}
	if typ != typeSubstr {
		for _, r := range val {
			if r > 127 {
				return domainEntry{}, fmt.Errorf("не-ASCII домен: запиши его в punycode (мойсайт.рф -> xn--...), конвертер https://www.punycoder.com/")
			}
		}
		if !domainRe.MatchString(val) {
			return domainEntry{}, fmt.Errorf("это не похоже на домен")
		}
		if !strings.Contains(val, ".") {
			fmt.Fprintf(os.Stderr, "warning: %q без точки — точно домен, а не опечатка?\n", val)
		}
	}
	return domainEntry{typ: typ, value: val, attrs: attrs}, nil
}

// cleanHost вытаскивает голое имя хоста: убирает схему, путь, порт, "*." и точки по краям.
func cleanHost(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if i := strings.Index(v, "://"); i >= 0 {
		v = v[i+3:]
	}
	if i := strings.IndexAny(v, "/?"); i >= 0 {
		v = v[:i]
	}
	if i := strings.LastIndex(v, "@"); i >= 0 {
		v = v[i+1:]
	}
	if i := strings.LastIndex(v, ":"); i >= 0 {
		if _, err := strconv.Atoi(v[i+1:]); err == nil {
			v = v[:i]
		}
	}
	v = strings.TrimPrefix(v, "*.")
	return strings.Trim(v, ".")
}

func parseCIDR(line string) (cidrEntry, error) {
	s := strings.Fields(line)[0]
	if !strings.Contains(s, "/") {
		ip := net.ParseIP(s)
		if ip == nil {
			return cidrEntry{}, fmt.Errorf("не IP-адрес и не CIDR")
		}
		if v4 := ip.To4(); v4 != nil {
			return cidrEntry{ip: v4, prefix: 32, text: s + "/32"}, nil
		}
		return cidrEntry{ip: ip.To16(), prefix: 128, text: s + "/128"}, nil
	}
	_, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return cidrEntry{}, fmt.Errorf("некорректный CIDR: %v", err)
	}
	ones, _ := ipnet.Mask.Size()
	if v4 := ipnet.IP.To4(); v4 != nil {
		return cidrEntry{ip: v4, prefix: uint32(ones), text: ipnet.String()}, nil
	}
	return cidrEntry{ip: ipnet.IP.To16(), prefix: uint32(ones), text: ipnet.String()}, nil
}

// ---------- include + дедупликация ----------

func resolveSites(code string, all []*listFile, seen map[string]bool) ([]domainEntry, error) {
	l := findList(code, all)
	if l == nil {
		return nil, fmt.Errorf("include: категория %s не найдена в data/geosite", code)
	}
	if seen[code] {
		return nil, fmt.Errorf("циклический include вокруг %s", code)
	}
	seen[code] = true
	defer delete(seen, code)

	var out []domainEntry
	idx := map[string]int{}
	add := func(d domainEntry) {
		k := fmt.Sprintf("%d\x00%s", d.typ, d.value)
		if i, ok := idx[k]; ok {
			out[i].attrs = mergeAttrs(out[i].attrs, d.attrs)
			return
		}
		idx[k] = len(out)
		out = append(out, d)
	}
	for _, inc := range l.includes {
		sub, err := resolveSites(inc, all, seen)
		if err != nil {
			return nil, fmt.Errorf("%s -> %w", code, err)
		}
		for _, d := range sub {
			add(d)
		}
	}
	for _, d := range l.sites {
		add(d)
	}
	return out, nil
}

func resolveIPs(code string, all []*listFile, seen map[string]bool) ([]cidrEntry, error) {
	l := findList(code, all)
	if l == nil {
		return nil, fmt.Errorf("include: категория %s не найдена в data/geoip", code)
	}
	if seen[code] {
		return nil, fmt.Errorf("циклический include вокруг %s", code)
	}
	seen[code] = true
	defer delete(seen, code)

	var out []cidrEntry
	idx := map[string]bool{}
	add := func(c cidrEntry) {
		if idx[c.text] {
			return
		}
		idx[c.text] = true
		out = append(out, c)
	}
	for _, inc := range l.includes {
		sub, err := resolveIPs(inc, all, seen)
		if err != nil {
			return nil, fmt.Errorf("%s -> %w", code, err)
		}
		for _, c := range sub {
			add(c)
		}
	}
	for _, c := range l.cidrs {
		add(c)
	}
	return out, nil
}

func findList(code string, all []*listFile) *listFile {
	for _, l := range all {
		if l.code == code {
			return l
		}
	}
	return nil
}

func mergeAttrs(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range append(append([]string{}, a...), b...) {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// ---------- protobuf (ручная сборка) ----------

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

// Domain{ type=1, value=2, attribute=3{ key=1, bool_value=2 } }
func encodeDomain(d domainEntry) []byte {
	var b []byte
	if d.typ != 0 {
		b = appendVarintField(b, 1, uint64(d.typ))
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

// GeoSite{ code=1, domain=2 } — code обязан идти первым: Xray ищет запись
// по префиксу сообщения, не разбирая его целиком.
func encodeGeoSite(code string, doms []domainEntry) []byte {
	var b []byte
	b = appendBytesField(b, 1, []byte(code))
	for _, d := range doms {
		b = appendBytesField(b, 2, encodeDomain(d))
	}
	return b
}

// GeoIP{ code=1, cidr=2{ ip=1, prefix=2 } }
func encodeGeoIP(code string, cidrs []cidrEntry) []byte {
	var b []byte
	b = appendBytesField(b, 1, []byte(code))
	for _, c := range cidrs {
		var cb []byte
		cb = appendBytesField(cb, 1, c.ip)
		if c.prefix != 0 {
			cb = appendVarintField(cb, 2, uint64(c.prefix))
		}
		b = appendBytesField(b, 2, cb)
	}
	return b
}

// ---------- экспорт для клиентов ----------

func clashSite(doms []domainEntry) string {
	var b strings.Builder
	for _, d := range doms {
		switch d.typ {
		case typeDomain:
			fmt.Fprintf(&b, "DOMAIN-SUFFIX,%s\n", d.value)
		case typeFull:
			fmt.Fprintf(&b, "DOMAIN,%s\n", d.value)
		case typeSubstr:
			fmt.Fprintf(&b, "DOMAIN-KEYWORD,%s\n", d.value)
		case typeRegex:
			fmt.Fprintf(&b, "DOMAIN-REGEX,%s\n", d.value)
		}
	}
	return b.String()
}

func clashIP(cidrs []cidrEntry) string {
	var b strings.Builder
	for _, c := range cidrs {
		kind := "IP-CIDR"
		if len(c.ip) == 16 {
			kind = "IP-CIDR6"
		}
		fmt.Fprintf(&b, "%s,%s,no-resolve\n", kind, c.text)
	}
	return b.String()
}

type sbRule struct {
	Domain        []string `json:"domain,omitempty"`
	DomainSuffix  []string `json:"domain_suffix,omitempty"`
	DomainKeyword []string `json:"domain_keyword,omitempty"`
	DomainRegex   []string `json:"domain_regex,omitempty"`
	IPCIDR        []string `json:"ip_cidr,omitempty"`
}

type sbSet struct {
	Version int      `json:"version"`
	Rules   []sbRule `json:"rules"`
}

func singboxSite(doms []domainEntry) []byte {
	var r sbRule
	for _, d := range doms {
		switch d.typ {
		case typeDomain:
			// в sing-box domain_suffix ".x" ловит только поддомены — сам домен добавляем отдельно
			r.Domain = append(r.Domain, d.value)
			r.DomainSuffix = append(r.DomainSuffix, "."+d.value)
		case typeFull:
			r.Domain = append(r.Domain, d.value)
		case typeSubstr:
			r.DomainKeyword = append(r.DomainKeyword, d.value)
		case typeRegex:
			r.DomainRegex = append(r.DomainRegex, d.value)
		}
	}
	out, _ := json.MarshalIndent(sbSet{Version: 1, Rules: []sbRule{r}}, "", "  ")
	return append(out, '\n')
}

func singboxIP(cidrs []cidrEntry) []byte {
	var r sbRule
	for _, c := range cidrs {
		r.IPCIDR = append(r.IPCIDR, c.text)
	}
	out, _ := json.MarshalIndent(sbSet{Version: 1, Rules: []sbRule{r}}, "", "  ")
	return append(out, '\n')
}

// ---------- мелочи ----------

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}
