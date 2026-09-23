// Package tmpl 提供「类 FreeMarker」的模板渲染：Go text/template + 自定义函数。
//
// 内置模板随二进制打包（go:embed templates/），按「分类/模板名」索引；
// 子功能可通过 output.template-dir 指定外部目录，用同名 .tpl 覆盖内置模板，
// 无需重新编译即可调整输出格式。
package tmpl

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"
)

// templates 内置模板库：templates/<category>/<name>.tpl
//
//go:embed all:templates
var templates embed.FS

// Engine 模板渲染引擎；dir 为空时只使用内置模板。
type Engine struct {
	dir string
}

// New 创建引擎；dir 为可选的外部模板目录。
func New(dir string) *Engine {
	return &Engine{dir: strings.TrimSpace(dir)}
}

// 模板可用函数（对应 FreeMarker 常用内建函数）。
var funcs = template.FuncMap{
	"repeat":  strings.Repeat,
	"indent":  func(n int) string { return strings.Repeat("  ", max0(n)) },
	"add":     func(a, b int) int { return a + b },
	"sub":     func(a, b int) int { return a - b },
	"pad":     func(n int, s string) string { return fmt.Sprintf("%-*s", n, s) },
	"padLeft": func(n int, s string) string { return fmt.Sprintf("%*s", n, s) },
	"trim":    strings.TrimSpace,
	"join":    strings.Join,
	"upper":   strings.ToUpper,
	"lower":   strings.ToLower,
	"now":     func(layout string) string { return time.Now().Format(layout) },
	// default：空值/空串回退到默认值
	"default": func(def, v any) any {
		if v == nil {
			return def
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			return def
		}
		return v
	},
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// Render 按「分类 + 模板名」渲染数据，返回文本结果。
func (e *Engine) Render(category, name string, data any) (string, error) {
	if name == "" {
		name = "markdown"
	}
	text, err := e.load(category, name)
	if err != nil {
		return "", err
	}
	t, err := template.New(name).Funcs(funcs).Parse(text)
	if err != nil {
		return "", fmt.Errorf("parse template %s/%s: %w", category, name, err)
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("render template %s/%s: %w", category, name, err)
	}
	return b.String(), nil
}

// List 返回某分类下可用的模板名（外部目录优先覆盖同名项）。
func (e *Engine) List(category string) []string {
	names := map[string]bool{}
	if entries, err := fs.ReadDir(templates, path.Join("templates", category)); err == nil {
		for _, en := range entries {
			if n := trimExt(en.Name()); n != "" {
				names[n] = true
			}
		}
	}
	if e.dir != "" {
		if entries, err := os.ReadDir(e.dir); err == nil {
			for _, en := range entries {
				if n := trimExt(en.Name()); n != "" {
					names[n] = true
				}
			}
		}
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// load 读取模板内容：外部目录优先，其次内置。
func (e *Engine) load(category, name string) (string, error) {
	base := name + ".tpl"
	if !strings.Contains(name, "..") && e.dir != "" {
		if b, err := os.ReadFile(filepath.Join(e.dir, base)); err == nil {
			return string(b), nil
		}
	}
	p := path.Join("templates", category, base)
	b, err := fs.ReadFile(templates, p)
	if err != nil {
		return "", fmt.Errorf("模板 %q 不存在（category=%s）: %w", name, category, err)
	}
	return string(b), nil
}

// trimExt 去掉 .tpl 后缀得到模板名。
func trimExt(name string) string {
	if strings.HasSuffix(strings.ToLower(name), ".tpl") {
		return name[:len(name)-len(".tpl")]
	}
	return ""
}
