package ui

import "embed"

//go:embed templates/*.tmpl
var TemplatesFS embed.FS

//go:embed static/*
var StaticFS embed.FS
