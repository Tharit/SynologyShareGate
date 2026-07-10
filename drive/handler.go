package drive

import "net/http"

// HandlePage serves the /drive/{id} user-facing page. Planned for v2.
func HandlePage(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Synology Drive sharing is not yet implemented.", http.StatusNotImplemented)
}

// HandleAPI serves /api/drive/* endpoints. Planned for v2.
func HandleAPI(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Synology Drive API is not yet implemented.", http.StatusNotImplemented)
}
