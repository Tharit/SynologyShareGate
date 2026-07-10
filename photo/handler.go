package photo

import "net/http"

// HandlePage serves the /photo/{id} user-facing page. Planned for v2.
func HandlePage(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Synology Photos sharing is not yet implemented.", http.StatusNotImplemented)
}

// HandleAPI serves /api/photo/* endpoints. Planned for v2.
func HandleAPI(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Synology Photos API is not yet implemented.", http.StatusNotImplemented)
}
