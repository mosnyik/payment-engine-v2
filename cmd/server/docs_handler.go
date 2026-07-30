package main

import (
	"net/http"
	"os"
)

// docsHandlers serves the hand-maintained OpenAPI spec (docs/openapi.yaml)
// and a Swagger UI page to browse it — mounted outside /v2 since it isn't
// a versioned API endpoint, and skipped entirely in production (see
// buildRouter) since it reads the spec from disk rather than embedding it.
type docsHandlers struct{}

func (docsHandlers) spec(w http.ResponseWriter, r *http.Request) {
	body, err := os.ReadFile("docs/openapi.yaml")
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(body)
}

func (docsHandlers) ui(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte(swaggerUIPage))
}

// swaggerUIPage loads Swagger UI from a CDN rather than vendoring it —
// this route is dev/staging tooling, not something bundled into the
// production binary.
const swaggerUIPage = `<!DOCTYPE html>
<html>
<head>
  <title>Payment Rail v2 API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    SwaggerUIBundle({
      url: "/docs/openapi.yaml",
      dom_id: "#swagger-ui",
    });
  </script>
</body>
</html>`
