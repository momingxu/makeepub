package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

////////////////////////////////////////////////////////////////////////////////

type TraverseFunc func(path string) error

type FileProvider interface {
	OpenFile(path string) (io.ReadCloser, error)
	Traverse(traverseFn TraverseFunc) error
}

////////////////////////////////////////////////////////////////////////////////

type FolderSource struct {
	base string
}

func NewFolderSource(base string) *FolderSource {
	fs := new(FolderSource)
	fs.base = base
	return fs
}

func (fs *FolderSource) OpenFile(path string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(fs.base, path))
}

func (fs *FolderSource) Traverse(traverseFn TraverseFunc) error {
	walk := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		path, _ = filepath.Rel(fs.base, path)
		return traverseFn(path)
	}

	return filepath.Walk(fs.base, walk)
}

////////////////////////////////////////////////////////////////////////////////

type ZipSource struct {
	rc *zip.ReadCloser
}

func NewZipSource(path string) (*ZipSource, error) {
	rc, e := zip.OpenReader(path)
	if e != nil {
		return nil, e
	}
	zs := new(ZipSource)
	zs.rc = rc
	return zs, nil
}

func (zs *ZipSource) Close() {
	zs.rc.Close()
}

func (zs *ZipSource) OpenFile(path string) (io.ReadCloser, error) {
	for _, f := range zs.rc.File {
		if strings.ToLower(f.Name) == path {
			return f.Open()
		}
	}
	return nil, os.ErrNotExist
}

func (zs *ZipSource) Traverse(traverseFn TraverseFunc) error {
	for _, f := range zs.rc.File {
		if e := traverseFn(f.Name); e != nil {
			return e
		}
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////////	

var (
	reHeader = regexp.MustCompile("^[ \t]*<[hH]([1-6])[^>]*>([^<]*)</[hH]([1-6])>[ \t]*$")
	reBody   = regexp.MustCompile("^[ \t]*<(?i)body(?-i)[^>]*>$")
)

func setCoverPage(book *Epub, fp FileProvider) error {
	f, e := fp.OpenFile("cover.html")
	if e != nil {
		return e
	}
	defer f.Close()

	if data, e := ioutil.ReadAll(f); e == nil {
		book.SetCoverPage("cover.html", data)
	}

	return e
}

func addFilesToBook(book *Epub, fp FileProvider) error {
	traverse := func(path string) error {
		p := strings.ToLower(path)
		if p == "book.ini" || p == "book.html" || p == "cover.html" {
			return nil
		}

		rc, e := fp.OpenFile(path)
		if e != nil {
			return e
		}
		defer rc.Close()
		data, e := ioutil.ReadAll(rc)
		if e != nil {
			return e
		}

		return book.AddFile(path, data)
	}

	return fp.Traverse(traverse)
}

func checkNewChapter(l string) (depth int, title string) {
	if m := reHeader.FindStringSubmatch(l); m != nil && m[1] == m[3] {
		depth = int(m[1][0] - '0')
		title = m[2]
	}
	return
}

func addChaptersToBook(book *Epub, fp FileProvider, maxDepth int) error {
	f, e := fp.OpenFile("book.html")
	if e != nil {
		return e
	}
	defer f.Close()
	br := bufio.NewReader(f)

	header := ""
	for {
		s, _, e := br.ReadLine()
		if e != nil {
			return e
		}
		l := string(s)
		header += l + "\n"
		if reBody.MatchString(l) {
			break
		}
	}

	buf := new(bytes.Buffer)
	depth, title := 1, ""
	for {
		s, _, e := br.ReadLine()
		if e == io.EOF {
			break
		}
		l := string(s)
		if nd, nt := checkNewChapter(l); nd > 0 && nd <= maxDepth {
			if buf.Len() > 0 {
				buf.WriteString("	</body>\n</html>")
				if e = book.AddChapter(title, buf.Bytes(), depth); e != nil {
					return e
				}
				buf.Reset()
			}
			depth, title = nd, nt
			buf.WriteString(header)
		}

		buf.WriteString(l + "\n")
	}

	if buf.Len() > 0 {
		e = book.AddChapter(title, buf.Bytes(), depth)
	}

	return nil
}

func loadConfig(fp FileProvider) (*Config, error) {
	rc, e := fp.OpenFile("book.ini")
	if e != nil {
		return nil, e
	}
	defer rc.Close()
	return ParseIni(rc)
}

func generateBook(fp FileProvider) error {
	cfg, e := loadConfig(fp)
	if e != nil {
		fmt.Println("Error: failed to open 'book.ini'")
		return e
	}

	s := cfg.GetString("/book/id", "")
	book, e := NewEpub(s)
	if e != nil {
		fmt.Println("Error: failed to create epub book.")
		return e
	}

	s = cfg.GetString("/book/name", "")
	if len(s) == 0 {
		fmt.Println("Warning: book name is empty.")
	}
	book.SetName(s)

	s = cfg.GetString("/book/author", "")
	if len(s) == 0 {
		fmt.Println("Warning: author name is empty.")
	}
	book.SetAuthor(s)

	if e = setCoverPage(book, fp); e != nil {
		fmt.Println("Error: failed to set cover page.")
		return e
	}

	if e = addFilesToBook(book, fp); e != nil {
		fmt.Println("Error: failed to add files to book.")
		return e
	}

	depth := cfg.GetInt("/book/depth", 1)
	if depth < 1 || depth > book.MaxDepth() {
		fmt.Println("Warning: invalid 'depth' value, reset to '1'")
		depth = 1
	}
	if e = addChaptersToBook(book, fp, depth); e != nil {
		fmt.Println("Error: failed to add chapters to book.")
		return e
	}

	s = cfg.GetString("/output/path", "")
	if len(os.Args) >= 3 {
		s = os.Args[2]
	}
	if len(s) == 0 {
		fmt.Println("Warning: output path has not set.")
	} else if e = book.Save(s); e != nil {
		fmt.Println("Error: failed to create output file.")
		return e
	}

	return nil
}

func isDir(name string) (bool, error) {
	stat, e := os.Stat(name)
	if e != nil {
		return false, e
	}
	return stat.IsDir(), nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage:\tmakeepub folder [output]\n\tmakeepub zipfile [output]")
		os.Exit(1)
	}

	start := time.Now()

	isdir, e := isDir(os.Args[1])
	if e != nil {
		fmt.Println("Error: failed to get source folder/file information.")
		os.Exit(1)
	}

	if isdir {
		fs := NewFolderSource(os.Args[1])
		e = generateBook(fs)
	} else {
		zs, err := NewZipSource(os.Args[1])
		if err == nil {
			defer zs.Close()
			e = generateBook(zs)
		} else {
			fmt.Println("Error: failed to open source zip file.")
			e = err
		}
	}

	if e != nil {
		os.Exit(1)
	}

	fmt.Println("Done, time used:", time.Now().Sub(start).String())
	os.Exit(0)
}
