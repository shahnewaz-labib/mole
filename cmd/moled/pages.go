package main

import (
	"fmt"
	"net/http"
)

// Tiny built-in error pages — mole's answer to Cloudflare's 404/502 pages.
// Visitors get real HTTP status codes now that the relay speaks HTTP.

func notFound(w http.ResponseWriter) {
	page(w, http.StatusNotFound, "No tunnel here",
		"There isn't a mole tunnel at this address.")
}

func badGateway(w http.ResponseWriter) {
	page(w, http.StatusBadGateway, "Origin unreachable",
		"The tunnel is registered but its origin service did not respond.")
}

func page(w http.ResponseWriter, code int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><html><head><title>%d %s · mole</title></head>
<body style="font-family:system-ui;max-width:32rem;margin:8rem auto;text-align:center">
<h1>%d — %s</h1><p style="color:#555">%s</p>
<hr style="border:none;border-top:1px solid #ddd"><small>mole</small></body></html>`,
		code, title, code, title, msg)
}
