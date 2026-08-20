// dupcheck ищет повторы в исходных списках data/geosite и data/geoip, чтобы
// сборщику .dat не приходилось чистить их на лету:
//
//   - точный дубликат (та же запись того же типа);
//   - запись, перекрытая более широкой: поддомен под domain:, full: под
//     domain:, что угодно под keyword:, подсеть внутри сети;
//   - повторный include: одной и той же категории.
//
// Строка разбирается ровно так же, как в geogen (комментарии, префиксы
// domain:/full:/keyword:/regexp:, чистка схемы/порта/«*.»), поэтому найденное
// здесь — это ровно то, что иначе схлопнулось бы при сборке .dat.
//
// Списки, подключённые через include:, проверяются вместе с подключившим:
// запись остаётся в исходной категории, а её копия у включающего помечается.
//
//	go run ./tools/dupcheck            # проверить, код возврата 1 если что-то нашлось
//	go run ./tools/dupcheck -fix       # вычистить найденное прямо в .txt
//	go run ./tools/dupcheck -cross     # + показать записи, попавшие в разные категории
package main

import (
	"bufio"
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

const (
	tDomain  = "domain"
	tFull    = "full"
	tKeyword = "keyword"
	tRegex   = "regexp"
	tCIDR    = "cidr"
	tInclude = "include"
)

type entry struct {
	file  string
	line  int
	typ   string
	val   string   // домен / подстрока / регулярка / нормализованный CIDR / код категории
	attrs []string // @атрибуты
	ip    net.IP   // только для tCIDR: адрес сети
	pfx   int      // только для tCIDR: длина префикса
	bits  int      // только для tCIDR: 32 или 128
}

func (e entry) key() string { return e.typ + "\x00" + e.val }

func (e entry) loc() string {
	return fmt.Sprintf("%s:%d", filepath.ToSlash(e.file), e.line)
}

// label — как запись выглядит в отчёте.
func (e entry) label() string {
	switch e.typ {
	case tDomain, tCIDR:
		return e.val
	default:
		return e.typ + ":" + e.val
	}
}

type listFile struct {
	code     string
	path     string
	includes []entry
	entries  []entry
}

type finding struct {
	e        entry
	reason   string
	attrLoss bool // у копии есть @атрибуты, которых нет у оставляемой записи
}

var (
	codeRe   = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_-]*$`)
	domainRe = regexp.MustCompile(`^[a-z0-9_*-]+(\.[a-z0-9_*-]+)*$`)
)

func main() {
	dataDir := flag.String("data", "data", "каталог с исходными списками (data/geosite, data/geoip)")
	fix := flag.Bool("fix", false, "удалить найденные дубликаты прямо в .txt")
	cross := flag.Bool("cross", false, "показать записи, попавшие сразу в несколько несвязанных категорий")
	flag.Parse()

	sites, err := loadDir(filepath.Join(*dataDir, "geosite"), false)
	fatal(err)
	ips, err := loadDir(filepath.Join(*dataDir, "geoip"), true)
	fatal(err)
	if len(sites)+len(ips) == 0 {
		fatal(fmt.Errorf("не найдено ни одного списка в %s", *dataDir))
	}

	f := &finder{found: map[string]finding{}}
	f.checkIncludes(sites)
	f.checkIncludes(ips)
	for _, l := range sites {
		f.scanSites(resolve(l, sites))
	}
	for _, l := range ips {
		f.scanIPs(resolve(l, ips))
	}

	found := f.sorted()
	report(found)
	if *cross {
		reportCross(sites)
	}

	if len(found) == 0 {
		fmt.Println("дубликатов нет")
		return
	}
	if !*fix {
		fmt.Println("\nудалить всё это разом:  go run ./tools/dupcheck -fix")
		os.Exit(1)
	}

	left, err := applyFix(found)
	fatal(err)
	if left > 0 {
		fmt.Printf("\n%d записей оставлено как есть — у копии свои @атрибуты, реши руками\n", left)
		os.Exit(1)
	}
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
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".txt" && ext != ".list" && ext != "" {
			continue
		}
		code := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
		if !codeRe.MatchString(code) {
			continue // имя не годится для .dat — на это ругается geogen, не мы
		}
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
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()

	l := &listFile{code: code, path: path}
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	ln := 0
	for sc.Scan() {
		ln++
		line := stripComment(sc.Text())
		if line == "" {
			continue
		}
		e := entry{file: path, line: ln}

		if rest, ok := cutPrefix(line, "include:"); ok {
			inc := strings.ToUpper(strings.TrimSpace(rest))
			if !codeRe.MatchString(inc) {
				continue
			}
			e.typ, e.val = tInclude, inc
			l.includes = append(l.includes, e)
			continue
		}

		if isIP {
			ip, pfx, bits, text, err := parseCIDR(line)
			if err != nil {
				warn(path, ln, line, err)
				continue
			}
			e.typ, e.val, e.ip, e.pfx, e.bits = tCIDR, text, ip, pfx, bits
			l.entries = append(l.entries, e)
			continue
		}

		typ, val, attrs, err := parseDomain(line)
		if err != nil {
			warn(path, ln, line, err)
			continue
		}
		e.typ, e.val, e.attrs = typ, val, attrs
		l.entries = append(l.entries, e)
	}
	return l, sc.Err()
}

func warn(path string, ln int, line string, err error) {
	fmt.Fprintf(os.Stderr, "warning: %s:%d не разобрал (%v), пропускаю: %q\n",
		filepath.ToSlash(path), ln, err, line)
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

func parseDomain(line string) (typ, val string, attrs []string, err error) {
	fields := strings.Fields(line)
	head := fields[0]
	for _, f := range fields[1:] {
		if !strings.HasPrefix(f, "@") {
			return "", "", nil, fmt.Errorf("лишний текст %q", f)
		}
		if a := strings.ToLower(strings.TrimPrefix(f, "@")); a != "" {
			attrs = append(attrs, a)
		}
	}

	typ, val = tDomain, head
	if i := strings.Index(head, ":"); i > 0 {
		switch strings.ToLower(head[:i]) {
		case "domain":
			typ, val = tDomain, head[i+1:]
		case "full":
			typ, val = tFull, head[i+1:]
		case "keyword", "substr":
			typ, val = tKeyword, head[i+1:]
		case "regexp", "regex":
			typ, val = tRegex, head[i+1:]
		}
	}
	if val == "" {
		return "", "", nil, fmt.Errorf("пустое значение")
	}
	if typ == tRegex {
		return typ, val, attrs, nil
	}
	if typ == tKeyword {
		return typ, strings.ToLower(strings.TrimSpace(val)), attrs, nil
	}

	val = cleanHost(val)
	if val == "" || !domainRe.MatchString(val) {
		return "", "", nil, fmt.Errorf("это не похоже на домен")
	}
	return typ, val, attrs, nil
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

func parseCIDR(line string) (net.IP, int, int, string, error) {
	s := strings.Fields(line)[0]
	if !strings.Contains(s, "/") {
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, 0, 0, "", fmt.Errorf("не IP-адрес и не CIDR")
		}
		if v4 := ip.To4(); v4 != nil {
			return v4, 32, 32, s + "/32", nil
		}
		return ip.To16(), 128, 128, s + "/128", nil
	}
	_, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return nil, 0, 0, "", fmt.Errorf("некорректный CIDR: %v", err)
	}
	ones, bits := ipnet.Mask.Size()
	if v4 := ipnet.IP.To4(); v4 != nil {
		return v4, ones, 32, ipnet.String(), nil
	}
	return ipnet.IP.To16(), ones, bits, ipnet.String(), nil
}

// ---------- разворачивание include ----------

// resolve возвращает записи категории так же, как их увидит geogen:
// сначала подключённые списки, потом свои строки. Каждая категория попадает
// в результат один раз, сколько бы include: на неё ни вело (заодно это защита
// от цикла — на сам цикл ругнётся geogen).
func resolve(l *listFile, all []*listFile) []entry {
	pulled := map[string]bool{l.code: true}
	var walk func(*listFile) []entry
	walk = func(x *listFile) []entry {
		var out []entry
		for _, inc := range x.includes {
			if pulled[inc.val] {
				continue
			}
			pulled[inc.val] = true
			if sub := findList(inc.val, all); sub != nil {
				out = append(out, walk(sub)...)
			}
		}
		return append(out, x.entries...)
	}
	return walk(l)
}

func findList(code string, all []*listFile) *listFile {
	for _, l := range all {
		if l.code == code {
			return l
		}
	}
	return nil
}

// ---------- поиск повторов ----------

type finder struct {
	found map[string]finding // ключ file:line — одну строку показываем один раз
}

func (f *finder) add(e entry, reason string, attrLoss bool) {
	if _, ok := f.found[e.loc()]; ok {
		return
	}
	f.found[e.loc()] = finding{e: e, reason: reason, attrLoss: attrLoss}
}

func (f *finder) sorted() []finding {
	out := make([]finding, 0, len(f.found))
	for _, v := range f.found {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].e.file != out[j].e.file {
			return out[i].e.file < out[j].e.file
		}
		return out[i].e.line < out[j].e.line
	})
	return out
}

func (f *finder) checkIncludes(all []*listFile) {
	for _, l := range all {
		seen := map[string]entry{}
		for _, inc := range l.includes {
			if prev, ok := seen[inc.val]; ok {
				f.add(inc, "повторный include: уже был в "+prev.loc(), false)
				continue
			}
			seen[inc.val] = inc
		}
	}
}

func (f *finder) scanSites(entries []entry) {
	kept := f.dropExact(entries)

	// от широкого к узкому: keyword ловит всё -> domain (чем меньше меток,
	// тем шире) -> full. Тогда одного прохода хватает, чтобы поймать перекрытие
	// в обе стороны, в каком бы порядке строки ни лежали в файле.
	ord := make([]entry, len(kept))
	copy(ord, kept)
	sort.SliceStable(ord, func(i, j int) bool { return breadth(ord[i]) < breadth(ord[j]) })

	byDomain := map[string]entry{}
	var keywords []entry
	for _, e := range ord {
		switch e.typ {
		case tKeyword:
			if p, ok := coveringKeyword(keywords, e.val); ok {
				f.add(e, "перекрыт "+p.label()+" ("+p.loc()+")", attrLoss(p, e))
				continue
			}
			keywords = append(keywords, e)
		case tDomain, tFull:
			if p, ok := coveringSuffix(byDomain, e.val); ok {
				f.add(e, "перекрыт "+p.label()+" ("+p.loc()+")", attrLoss(p, e))
				continue
			}
			if p, ok := coveringKeyword(keywords, e.val); ok {
				f.add(e, "перекрыт "+p.label()+" ("+p.loc()+")", attrLoss(p, e))
				continue
			}
			if e.typ == tDomain {
				byDomain[e.val] = e
			}
		}
	}
}

func (f *finder) scanIPs(entries []entry) {
	kept := f.dropExact(entries)

	ord := make([]entry, len(kept))
	copy(ord, kept)
	sort.SliceStable(ord, func(i, j int) bool { return ord[i].pfx < ord[j].pfx }) // сначала широкие сети

	seen := map[string]entry{}
	for _, e := range ord {
		if p, ok := coveringNet(seen, e); ok {
			f.add(e, "входит в "+p.label()+" ("+p.loc()+")", false)
			continue
		}
		seen[netKey(e.ip, e.pfx, e.bits)] = e
	}
}

// dropExact помечает точные повторы (в порядке файла — остаётся первый)
// и возвращает уцелевшие записи.
func (f *finder) dropExact(entries []entry) []entry {
	exact := map[string]entry{}
	var kept []entry
	for _, e := range entries {
		if prev, ok := exact[e.key()]; ok {
			f.add(e, "точный дубликат "+prev.loc(), attrLoss(prev, e))
			continue
		}
		exact[e.key()] = e
		kept = append(kept, e)
	}
	return kept
}

func breadth(e entry) int {
	switch e.typ {
	case tKeyword:
		return 0
	case tDomain:
		return 1 + strings.Count(e.val, ".") // domain:foo.com шире, чем domain:x.foo.com
	case tFull:
		return 1 << 20
	default: // regexp — сравниваем только на точное совпадение
		return 1 << 21
	}
}

// coveringSuffix ищет domain:-запись, под которую подпадает val (сам домен или его родитель).
func coveringSuffix(byDomain map[string]entry, val string) (entry, bool) {
	for s := val; ; {
		if p, ok := byDomain[s]; ok {
			return p, true
		}
		i := strings.Index(s, ".")
		if i < 0 {
			return entry{}, false
		}
		s = s[i+1:]
	}
}

func coveringKeyword(keywords []entry, val string) (entry, bool) {
	for _, k := range keywords {
		if strings.Contains(val, k.val) {
			return k, true
		}
	}
	return entry{}, false
}

func coveringNet(seen map[string]entry, e entry) (entry, bool) {
	for q := 0; q < e.pfx; q++ {
		if p, ok := seen[netKey(e.ip.Mask(net.CIDRMask(q, e.bits)), q, e.bits)]; ok {
			return p, true
		}
	}
	return entry{}, false
}

func netKey(ip net.IP, pfx, bits int) string {
	return fmt.Sprintf("%d/%s/%d", bits, ip.String(), pfx)
}

// attrLoss — правда, если у копии есть @атрибут, которого нет у остающейся
// записи: geogen такие атрибуты сливает, поэтому просто удалить строку нельзя.
func attrLoss(keep, drop entry) bool {
	if len(drop.attrs) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, a := range keep.attrs {
		have[a] = true
	}
	for _, a := range drop.attrs {
		if !have[a] {
			return true
		}
	}
	return false
}

// ---------- отчёт ----------

func report(found []finding) {
	if len(found) == 0 {
		return
	}
	byFile := map[string][]finding{}
	var files []string
	for _, fd := range found {
		if _, ok := byFile[fd.e.file]; !ok {
			files = append(files, fd.e.file)
		}
		byFile[fd.e.file] = append(byFile[fd.e.file], fd)
	}
	sort.Strings(files)

	for _, file := range files {
		fmt.Printf("\n%s — %d\n", filepath.ToSlash(file), len(byFile[file]))
		for _, fd := range byFile[file] {
			mark := ""
			if fd.attrLoss {
				mark = "  [есть свои @атрибуты — руками]"
			}
			fmt.Printf("  %5d  %-40s %s%s\n", fd.e.line, fd.e.label(), fd.reason, mark)
		}
	}
	fmt.Printf("\nвсего лишних записей: %d в %d файлах\n", len(found), len(files))
}

// reportCross показывает записи, попавшие сразу в несколько категорий, не
// связанных include. Для .dat это не дубликат, но в роутинге сработает та
// категория, чьё правило стоит выше, — обычно это не то, чего хотели.
func reportCross(all []*listFile) {
	where := map[string][]string{}
	label := map[string]string{}
	for _, l := range all {
		seen := map[string]bool{}
		for _, e := range resolve(l, all) {
			if seen[e.key()] {
				continue
			}
			seen[e.key()] = true
			where[e.key()] = append(where[e.key()], l.code)
			label[e.key()] = e.label()
		}
	}
	closure := includeClosure(all)

	var lines []string
	for k, cats := range where {
		if len(cats) < 2 || !unrelated(cats, closure) {
			continue
		}
		sort.Strings(cats)
		lines = append(lines, fmt.Sprintf("  %-40s %s", label[k], strings.Join(cats, ", ")))
	}
	sort.Strings(lines)

	fmt.Printf("\n== одна запись в разных категориях: %d ==\n", len(lines))
	for i, s := range lines {
		if i == 50 {
			fmt.Printf("  ... и ещё %d\n", len(lines)-50)
			break
		}
		fmt.Println(s)
	}
}

func includeClosure(all []*listFile) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, l := range all {
		set := map[string]bool{}
		var walk func(*listFile)
		walk = func(x *listFile) {
			for _, inc := range x.includes {
				if set[inc.val] {
					continue
				}
				set[inc.val] = true
				if sub := findList(inc.val, all); sub != nil {
					walk(sub)
				}
			}
		}
		walk(l)
		out[l.code] = set
	}
	return out
}

// unrelated — правда, если среди категорий есть хотя бы две, не связанные include.
func unrelated(names []string, closure map[string]map[string]bool) bool {
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			a, b := names[i], names[j]
			if !closure[a][b] && !closure[b][a] {
				return true
			}
		}
	}
	return false
}

// ---------- -fix ----------

func applyFix(found []finding) (int, error) {
	drop := map[string]map[int]bool{}
	left := 0
	for _, fd := range found {
		if fd.attrLoss {
			left++
			continue
		}
		if drop[fd.e.file] == nil {
			drop[fd.e.file] = map[int]bool{}
		}
		drop[fd.e.file][fd.e.line] = true
	}

	var files []string
	for f := range drop {
		files = append(files, f)
	}
	sort.Strings(files)

	total := 0
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			return left, err
		}
		text := string(b)
		trailing := strings.HasSuffix(text, "\n")
		if trailing {
			text = text[:len(text)-1]
		}
		var out []string
		for i, line := range strings.Split(text, "\n") {
			if drop[file][i+1] {
				continue
			}
			out = append(out, line)
		}
		res := strings.Join(out, "\n")
		if trailing {
			res += "\n"
		}
		if err := os.WriteFile(file, []byte(res), 0o644); err != nil {
			return left, err
		}
		fmt.Printf("почищено %s: -%d строк\n", filepath.ToSlash(file), len(drop[file]))
		total += len(drop[file])
	}
	fmt.Printf("удалено записей: %d\n", total)
	return left, nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}
