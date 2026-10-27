package mcpapi

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
