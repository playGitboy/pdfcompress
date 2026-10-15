package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProbeRealCorpus 对真实/合成语料跑几何探测，打印结果供人工核对。
// 通过设置环境变量 PDFGEOM_CORPUS 指定语料目录（允许多个，逗号分隔）。
// 未设置时跳过，保证 CI/常规 go test 不受影响。
func TestProbeRealCorpus(t *testing.T) {
	env := os.Getenv("PDFGEOM_CORPUS")
	if env == "" {
		t.Skip("未设置 PDFGEOM_CORPUS，跳过真实语料探测")
	}
	dirs := filepath.SplitList(env)
	if len(dirs) == 1 && os.PathSeparator == '\\' {
		// Windows 下 SplitList 用 ';'，兼容用户用 ',' 分隔
		dirs = splitComma(env)
	}
	var files []string
	for _, d := range dirs {
		_ = filepath.Walk(d, func(p string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if filepath.Ext(p) == ".pdf" {
				files = append(files, p)
			}
			return nil
		})
	}
	if len(files) == 0 {
		t.Fatal("语料目录中未找到 PDF")
	}
	t.Logf("共 %d 个 PDF", len(files))
	for _, p := range files {
		g := probePDFGeom(p)
		kindStr := map[geomKind]string{
			geomStandard: "STANDARD", geomPixelAsPt: "PIXEL_AS_PT", geomUnknown: "UNKNOWN",
		}[g.kind]
		fix := "no"
		if g.shouldFixGeometry() {
			fix = "FIX->" + g.paper.name
		}
		t.Logf("%-40s pages=%-4d %-12s uniform=%-5v mixed=%-5v sample=%.0fx%.0f fix=%s",
			filepath.Base(p), g.pages, kindStr, g.uniform, g.mixedOrientation,
			g.sampleW, g.sampleH, fix)
	}
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
