// Package migrations 内嵌 SQL 迁移脚本，命名规则：<版本>_<描述>.up.sql / .down.sql。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
