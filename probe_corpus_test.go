package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestProbeCorpusDir 扫描测试目录下所有 PDF，输出几何判定与档位预期。
// 通过环境变量 PDFPROBE_DIR 指定目录；不设则用 ./test。
func TestProbeCorpusDir(t *testing.T) {
	dir := os.Getenv("PDFPROBE_DIR")
	if dir == "" {
		dir = "test"
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("skip: %v", err)
	}
	var files []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ".pdf" {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Strings(files)

	type row struct {
		name   string
		size   int64
		kind   string
		paper  string
		pages  int
		unif   bool
		mixed  bool
		hasImg bool
		fix    bool
		sw, sh float64
		tw, th float64
	}
	var rows []row
	nFix, nStd, nUnk := 0, 0, 0
	for _, f := range files {
		st, _ := os.Stat(f)
		g := probePDFGeom(f)
		r := row{
			name: filepath.Base(f), size: st.Size(),
			kind: geomKindName(g.kind), paper: g.paper.name,
			pages: g.pages, unif: g.uniform,
			mixed: g.mixedOrientation, hasImg: g.hasImage,
			fix: g.shouldFixGeometry(),
			sw:  g.sampleW, sh: g.sampleH,
		}
		if r.fix {
			r.tw, r.th = targetPageSize(g)
			nFix++
		} else if g.kind == geomStandard {
			nStd++
		} else {
			nUnk++
		}
		rows = append(rows, r)
	}

	fmt.Printf("\n%-16s %10s %-11s %-6s %4s %4s %4s %4s %4s  %-18s %s\n",
		"FILE", "SIZE_MB", "KIND", "PAPER", "PG", "UNI", "MIX", "IMG", "FIX", "SAMPLE(pt)", "TARGET(pt)")
	fmt.Println("------------------------------------------------------------------------------------------------------------------------")
	for _, r := range rows {
		fmt.Printf("%-16s %10.2f %-11s %-6s %4d %4s %4s %4s %4s  %-18s %s\n",
			r.name, float64(r.size)/1048576.0, r.kind, r.paper, r.pages,
			yn(r.unif), yn(r.mixed), yn(r.hasImg), yn(r.fix),
			fmt.Sprintf("%.0fx%.0f", r.sw, r.sh),
			func() string {
				if r.fix {
					return fmt.Sprintf("%.1fx%.1f", r.tw, r.th)
				}
				return "-"
			}())
	}
	fmt.Printf("\n合计: %d 文件 | STANDARD(不变) %d | PIXEL_AS_PT(修正) %d | UNKNOWN(保守) %d\n\n",
		len(rows), nStd, nFix, nUnk)
}

func yn(b bool) string {
	if b {
		return "Y"
	}
	return "-"
}

func geomKindName(k geomKind) string {
	switch k {
	case geomStandard:
		return "STANDARD"
	case geomPixelAsPt:
		return "PIXEL_AS_PT"
	default:
		return "UNKNOWN"
	}
}
