{{- /* 图书目录 · 纯文本大纲：编号左对齐、页码右对齐，适合直接粘进文档 */ -}}
{{ default "图书目录" .Title }}
{{ repeat "=" 32 }}

{{ range .Entries -}}
{{ indent (sub .Level 1) }}{{ if .Number }}{{ pad 10 .Number }} {{ end }}{{ .Title }}{{ if gt .Page 0 }}{{ padLeft 8 (printf "p.%d" .Page) }}{{ end }}
{{ end }}

{{ repeat "-" 32 }}
共 {{ len .Entries }} 条 · 来源 {{ .PageCount }} 页 · {{ .GeneratedAt }} · 模板 {{ .Template }}
