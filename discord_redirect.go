package main

import (
	"net/http"
	"strings"
)

// discordRedirect only accepts Discord channel and message snowflakes. It is
// not a general redirector: the target scheme and route are fixed.
func discordRedirect(w http.ResponseWriter, r *http.Request) {
	const prefix = "/mautrix-discord/open/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	valid := func(s string) bool { return s != "" && isNumber(s) }
	if len(parts) != 3 || (parts[0] != "@me" && !valid(parts[0])) || !valid(parts[1]) || !valid(parts[2]) || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, "discord:///channels/"+strings.Join(parts, "/"), http.StatusFound)
}
