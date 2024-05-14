package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/ast"
	mdhtml "github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"

	"github.com/kjk/common/u"
)

var logvf = logf

type MdProcessedInfo struct {
	mdFileName string
	data       []byte
}

// paths are relative to "docs" folder
var (
	mdDocsDir   = path.Join("md")
	mdProcessed = map[string]*MdProcessedInfo{}
	mdToProcess = []string{}
	mdHTMLExt   = true
	fsys        fs.FS
)

const h1BreadcrumbsEnd = `</div>
</div>
`

func getH1BreadcrumbStart() string {
	const h1BreadcrumbsStart = `
	<div class="breadcrumbs">
		<div><a href="SumatraPDF-documentation.html">SumatraPDF documentation</a></div>
		<div>/</div>
		<div>`
	const h1BreadcrumbsStartWebsite = `
<div class="breadcrumbs">
	<div><a href="SumatraPDF-documentation">SumatraPDF documentation</a></div>
	<div>/</div>
	<div>`
	if docsForWebsite {
		return h1BreadcrumbsStartWebsite
	}
	return h1BreadcrumbsStart
}

func renderFirstH1(w io.Writer, h *ast.Heading, entering bool, seenFirstH1 *bool) {
	if entering {
		io.WriteString(w, getH1BreadcrumbStart())
	} else {
		*seenFirstH1 = true
		io.WriteString(w, h1BreadcrumbsEnd)
	}
}

func genCsvTableHTML(records [][]string, noHeader bool) string {
	if len(records) == 0 {
		return ""
	}
	lines := []string{`<table class="collection-content">`}
	if !noHeader {
		row := records[0]
		records = records[1:]
		push(&lines, "<thead>", "<tr>")
		for _, cell := range row {
			s := fmt.Sprintf(`<th>%s</th>`, cell)
			push(&lines, s)
		}
		push(&lines, "</tr>", "</thead>")
	}

	push(&lines, "<tbody>")
	for len(records) > 0 {
		push(&lines, "<tr>")
		row := records[0]
		records = records[1:]
		for i, cell := range row {
			cell = strings.TrimSpace(cell)
			if cell == "" {
				push(&lines, "<td>", "</td>")
				continue
			}
			inCode := i == 0 || i == 1
			push(&lines, "<td>")
			if inCode {
				// TODO: "Ctrl + W, Ctrl + F4"
				// should be rendered as:
				// <code>Ctrl + W</code>,&nbsp;<code>Ctrl + F4</code>
				s := fmt.Sprintf("<code>%s</code>", cell)
				push(&lines, s)
			} else {
				push(&lines, cell)
			}
			push(&lines, "</td>")
		}
		push(&lines, "</tr>")
	}

	push(&lines, "</tbody>", "</table>")
	return strings.Join(lines, "\n")
}

func renderCodeBlock(w io.Writer, cb *ast.CodeBlock, entering bool) {
	csvContent := bytes.TrimSpace(cb.Literal)
	// os.WriteFile("temp.csv", csvContent, 0644)
	r := csv.NewReader(bytes.NewReader(csvContent))
	records, err := r.ReadAll()
	must(err)
	s := genCsvTableHTML(records, false)
	io.WriteString(w, s)
}

func renderColumns(w io.Writer, columns *Columns, entering bool) {
	if entering {
		io.WriteString(w, `<div class="doc-columns">`)
	} else {
		io.WriteString(w, `</div>`)
	}
}

func makeRenderHook(r *mdhtml.Renderer, isMainPage bool) mdhtml.RenderNodeFunc {
	seenFirstH1 := false
	return func(w io.Writer, node ast.Node, entering bool) (ast.WalkStatus, bool) {
		if !seenFirstH1 {
			if h, ok := node.(*ast.Heading); ok && h.Level == 1 {
				if isMainPage {
					seenFirstH1 = true
					return ast.SkipChildren, true
				}
				renderFirstH1(w, h, entering, &seenFirstH1)
				return ast.GoToNext, true
			}
		}
		if cb, ok := node.(*ast.CodeBlock); ok {
			if string(cb.Info) != "commands" {
				return ast.GoToNext, false
			}
			renderCodeBlock(w, cb, entering)
			return ast.GoToNext, true
		}
		if columns, ok := node.(*Columns); ok {
			renderColumns(w, columns, entering)
			return ast.GoToNext, true
		}
		return ast.GoToNext, false
	}
}

func newMarkdownHTMLRenderer(isMainPage bool) *mdhtml.Renderer {
	htmlFlags := mdhtml.Smartypants |
		mdhtml.SmartypantsFractions |
		mdhtml.SmartypantsDashes |
		mdhtml.SmartypantsLatexDashes
	htmlOpts := mdhtml.RendererOptions{
		Flags:        htmlFlags,
		ParagraphTag: "div",
	}
	r := mdhtml.NewRenderer(htmlOpts)
	r.Opts.RenderNodeHook = makeRenderHook(r, isMainPage)
	return r
}

type Columns struct {
	ast.Container
}

var columns = []byte(":columns\n")

func parseColumns(data []byte) (ast.Node, []byte, int) {
	if !bytes.HasPrefix(data, columns) {
		return nil, nil, 0
	}
	i := len(columns)
	// find empty line
	// TODO: should also consider end of document
	end := bytes.Index(data[i:], columns)
	if end < 0 {
		return nil, data, 0
	}
	inner := data[i : end+i]
	res := &Columns{}
	return res, inner, end + i + i
}

func parserHook(data []byte) (ast.Node, []byte, int) {
	if node, d, n := parseColumns(data); node != nil {
		return node, d, n
	}
	return nil, nil, 0
}

func newMarkdownParser() *parser.Parser {
	extensions := parser.NoIntraEmphasis |
		parser.Tables |
		parser.FencedCode |
		parser.Autolink |
		parser.Strikethrough |
		parser.SpaceHeadings |
		parser.NoEmptyLineBeforeBlock |
		parser.AutoHeadingIDs

	p := parser.NewWithExtensions(extensions)
	p.Opts.ParserHook = parserHook
	return p
}

func getFileExt(s string) string {
	ext := filepath.Ext(s)
	return strings.ToLower(ext)
}

func removeNotionId(s string) string {
	if len(s) <= 32 {
		return s
	}
	isHex := func(c rune) bool {
		if c >= '0' && c <= '9' {
			return true
		}
		if c >= 'a' && c <= 'f' {
			return true
		}
		if c >= 'A' && c <= 'F' {
			return true
		}
		return false
	}
	suffix := s[len(s)-32:]
	for _, c := range suffix {
		if !isHex(c) {
			return s
		}
	}
	return s[:len(s)-32]
}

func getHTMLFileName(mdName string) string {
	parts := strings.Split(mdName, ".")
	panicIf(len(parts) != 2)
	panicIf(parts[1] != "md")
	name := parts[0]
	name = removeNotionId(name)
	name = strings.TrimSpace(name)
	name = strings.Replace(name, " ", "-", -1)
	if mdHTMLExt {
		name += ".html"
	}
	return name
}

func FsFileExistsMust(fsys fs.FS, name string) {
	_, err := fsys.Open(name)
	must(err)
}

func checkMdFileExistsMust(name string) {
	path := path.Join(mdDocsDir, name)
	FsFileExistsMust(fsys, path)
}

func astWalk(doc ast.Node) {
	ast.WalkFunc(doc, func(node ast.Node, entering bool) ast.WalkStatus {
		if img, ok := node.(*ast.Image); ok && entering {
			uri := string(img.Destination)
			if strings.HasPrefix(uri, "https://") {
				return ast.GoToNext
			}
			logf("  img.Destination:  %s\n", string(uri))
			fileName := strings.Replace(uri, "%20", " ", -1)
			checkMdFileExistsMust(fileName)
			img.Destination = []byte(fileName)
			return ast.GoToNext
		}

		if link, ok := node.(*ast.Link); ok && entering {
			uri := string(link.Destination)
			isExternalURI := func(uri string) bool {
				return (strings.HasPrefix(uri, "https://") || strings.HasPrefix(uri, "http://")) && !strings.Contains(uri, "sumatrapdfreader.org")
			}
			if isExternalURI(string(link.Destination)) {
				link.AdditionalAttributes = append(link.AdditionalAttributes, `target="_blank"`)
			}

			if strings.HasPrefix(uri, "https://") {
				return ast.GoToNext
			}
			// TODO: change to https://
			if strings.HasPrefix(uri, "http://") {
				return ast.GoToNext
			}
			if strings.HasPrefix(uri, "mailto:") {
				return ast.GoToNext
			}
			logvf("  link.Destination: %s\n", uri)
			fileName := strings.Replace(uri, "%20", " ", -1)
			logvf("  mdName          : %s\n", fileName)
			if strings.HasPrefix(fileName, "Untitled Database") {
				fileName = strings.Replace(fileName, ".md", ".csv", -1)
				logvf("  mdName          : %s\n", fileName)
				return ast.GoToNext
			}

			checkMdFileExistsMust(fileName)
			ext := getFileExt(fileName)
			if ext == ".png" || ext == ".jpg" || ext == ".jpeg" {
				return ast.GoToNext
			}
			if ext == ".csv" {
				return ast.GoToNext
			}
			panicIf(ext != ".md")
			push(&mdToProcess, fileName)
			link.Destination = []byte(getHTMLFileName(fileName))
		}

		return ast.GoToNext
	})
}

var (
	muMdToHTML sync.Mutex
)

func mdToHTML(name string, force bool) ([]byte, error) {
	name = strings.TrimPrefix(name, "docs-md/")
	logvf("mdToHTML: '%s', force: %v\n", name, force)
	isMainPage := name == "SumatraPDF-documentation.md"

	// called from http goroutines so needs to be thread-safe
	muMdToHTML.Lock()
	defer muMdToHTML.Unlock()

	mdInfo := mdProcessed[name]
	if mdInfo != nil && !force {
		logvf("mdToHTML: skipping '%s' because already processed\n", name)
		return mdInfo.data, nil
	}
	logvf("mdToHTML: processing '%s'\n", name)
	mdInfo = &MdProcessedInfo{
		mdFileName: name,
	}
	mdProcessed[name] = mdInfo

	filePath := path.Join(mdDocsDir, name)
	md, err := fs.ReadFile(fsys, filePath)
	if err != nil {
		return nil, err
	}
	logf("read:  %s size: %s\n", filePath, u.FormatSize(int64(len(md))))
	parser := newMarkdownParser()
	renderer := newMarkdownHTMLRenderer(isMainPage)
	doc := parser.Parse(md)
	astWalk(doc)
	res := markdown.Render(doc, renderer)
	innerHTML := string(res)

	innerHTML = `<div class="notion-page">` + innerHTML + `</div>`
	innerHTML += `<hr>`
	editLink := `<center><a href="https://github.com/sumatrapdfreader/sumatrapdf/blob/master/docs/md/{name}" target="_blank" class="suggest-change">edit</a></center>`
	editLink = strings.Replace(editLink, "{name}", name, -1)
	innerHTML += editLink
	filePath = "manual.tmpl.html"
	if docsForWebsite {
		filePath = "manual.website.tmpl.html"
	}
	tmplManual, err := fs.ReadFile(fsys, filePath)
	must(err)
	s := strings.Replace(string(tmplManual), "{{InnerHTML}}", innerHTML, -1)
	title := getHTMLFileName(name)
	title = strings.Replace(title, ".html", "", -1)
	title = strings.Replace(title, "-", " ", -1)
	s = strings.Replace(s, "{{Title}}", title, -1)

	panicIf(searchJS == "")
	if name == "Commands.md" {
		s = strings.Replace(s, `<div>:search:</div>`, searchHTML, -1)
		toReplace := "</body>"
		s = strings.Replace(s, toReplace, searchJS+toReplace, 1)
	}
	mdInfo.data = []byte(s)
	return mdInfo.data, nil
}

var (
	// if true, we generate docs for website, which requires slight changes
	docsForWebsite = false
)

var searchJS = ``
var searchHTML = ``

func loadSearchJS() {
	{
		path := filepath.Join("do", "gen_docs.search.js")
		d, err := os.ReadFile(path)
		must(err)
		searchJS = `<script>` + string(d) + `</script>`
	}
	{
		path := filepath.Join("do", "gen_docs.search.html")
		d, err := os.ReadFile(path)
		must(err)
		searchHTML = string(d)
	}
}

func removeHTMLFilesInDir(dir string) {
	files, err := os.ReadDir(dir)
	must(err)
	for _, fi := range files {
		if fi.IsDir() {
			continue
		}
		name := fi.Name()
		if strings.HasSuffix(name, ".html") {
			path := filepath.Join(dir, name)
			must(os.Remove(path))
		}
	}
}

func writeDocsHtmlFiles() {
	wwwDir := filepath.Join("docs", "www")
	imgDir := filepath.Join(wwwDir, "img")
	// images are copied from docs/md/img so remove potentially stale images
	must(os.RemoveAll(imgDir))
	must(os.MkdirAll(filepath.Join(wwwDir, "img"), 0755))
	// remove potentially stale .html files
	// can't just remove the directory because has .css and .ico files
	removeHTMLFilesInDir(wwwDir)
	for name, info := range mdProcessed {
		name = strings.ReplaceAll(name, ".md", ".html")
		path := filepath.Join(wwwDir, name)
		err := os.WriteFile(path, info.data, 0644)
		logf("wrote '%s', len: %d\n", path, len(info.data))
		must(err)
	}
	{
		// copy image files
		copyFileMustOverwrite = true
		dstDir := filepath.Join(wwwDir, "img")
		srcDir := filepath.Join("docs", "md", "img")
		copyFilesRecurMust(dstDir, srcDir)
	}
	{
		// create lzsa archive
		makeLzsa := filepath.Join("bin", "MakeLZSA.exe")
		archive := filepath.Join("docs", "manual.dat")
		os.Remove(archive)
		cmd := exec.Command(makeLzsa, archive, wwwDir)
		runCmdLoggedMust(cmd)
		size := u.FileSize(archive)
		sizeH := humanize.Bytes(uint64(size))
		logf("size of '%s': %s\n", archive, sizeH)
	}
	{
		dir, err := filepath.Abs(wwwDir)
		must(err)
		url := "file://" + filepath.Join(dir, "SumatraPDF-documentation.html")
		logf("To view, open:\n%s\n", url)
	}
}

func genHTMLDocsForWebsite() {
	logf("genHTMLDocsForWebsite starting\n")
	docsForWebsite = true
	dir := updateSumatraWebsite()
	if !u.DirExists(dir) {
		logFatalf("Directory '%s' doesn't exist\n", dir)
	}
	{
		cmd := exec.Command("git", "pull")
		cmd.Dir = dir
		runCmdMust(cmd)
	}
	// don't use .html extension in links to generated .html files
	// for docs we need them because they are shown from file system
	// for website we prefer "clean" links because they are served via web server
	mdHTMLExt = false
}

func genHTMLDocsFromMarkdown() {
	logf("genHTMLDocsFromMarkdown starting\n")
	timeStart := time.Now()
	loadSearchJS()
	fsys = os.DirFS("docs")

	mdToHTML("SumatraPDF-documentation.md", false)
	for len(mdToProcess) > 0 {
		name := mdToProcess[0]
		mdToProcess = mdToProcess[1:]
		_, err := mdToHTML(name, false)
		must(err)
	}
	writeDocsHtmlFiles()
	//u.OpenBrowser(filepath.Join("docs", "www", "SumatraPDF-documentation.html"))
	logf("genHTMLDocsFromMarkdown finished in %s\n", time.Since(timeStart))
}
