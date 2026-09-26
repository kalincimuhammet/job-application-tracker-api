// main.go
//
// Bewerbungs-API: Go-Port von api/applications.php.
// Speichert alle Bewerbungen in einer JSON-Datei. Nur Standardbibliothek, keine externen Abhängigkeiten.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// --- Konfiguration ---

// Entspricht REQUIRED_FIELDS aus dem PHP-Original.
var requiredFields = []string{"id", "address", "channel", "applicationDate", "salaryExpectation", "rejectionDate", "notes"}

// Schutz vor riesigen Request-Bodys (PHP hat dafür post_max_size, Go hat von sich aus kein Limit).
const maxBodyBytes = 5 << 20 // 5 MB

// Application ist bewusst eine generische Map statt eines Structs:
// So bleiben Felder mit wechselnden Typen (z.B. salaryExpectation als Zahl, Text oder null)
// und eventuell zusätzliche Felder aus dem Frontend unverändert erhalten - genau wie in PHP.
type Application map[string]any

// env liest eine Umgebungsvariable mit Fallback-Wert.
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// --- Hilfsfunktionen für JSON-Antworten ---

// writeJSON schreibt eine JSON-Antwort mit dem angegebenen HTTP-Statuscode.
// (Content-Type wird zentral in der Middleware gesetzt.)
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("Antwort konnte nicht geschrieben werden: %v", err)
	}
}

// writeError entspricht sendError() aus PHP: Fehlermeldung + Statuscode.
// Im Gegensatz zu PHP gibt es kein exit - der Handler muss danach selbst mit return abbrechen.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// decodeJSON dekodiert genau einen JSON-Wert. UseNumber() sorgt dafür, dass Zahlen (z.B. große IDs)
// nicht in float64 umgewandelt werden und beim Zurückschreiben exakt so bleiben, wie sie waren.
func decodeJSON(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// Nach dem JSON-Wert darf nichts Weiteres mehr kommen (json_decode in PHP wäre hier ebenfalls fehlgeschlagen).
	if dec.More() {
		return errors.New("unerwartete Daten nach dem JSON-Wert")
	}
	return nil
}

// decodeBody entspricht getJsonBody() aus PHP: liest den Request-Body und dekodiert ihn nach v.
// Bei Fehlern wird die passende Antwort (400/413) direkt geschrieben und false zurückgegeben.
// expectedMsg ist die Meldung, falls der JSON-Typ nicht passt (z.B. Objekt statt Array).
func decodeBody(w http.ResponseWriter, r *http.Request, v any, expectedMsg string) bool {
	err := decodeJSON(http.MaxBytesReader(w, r.Body, maxBodyBytes), v)
	if err == nil {
		return true
	}
	var tooBig *http.MaxBytesError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, "Request-Body zu groß")
	case errors.As(err, &typeErr):
		writeError(w, http.StatusBadRequest, expectedMsg)
	default:
		writeError(w, http.StatusBadRequest, "Ungültiges JSON: "+err.Error())
	}
	return false
}

// validateEntry prüft, ob ein einzelner Bewerbungs-Eintrag alle Pflichtfelder besitzt.
// Wird bei POST für jeden Eintrag im gesendeten Array aufgerufen.
func validateEntry(entry Application, index int) error {
	for _, field := range requiredFields {
		if _, ok := entry[field]; !ok {
			return fmt.Errorf("Eintrag %d: Feld '%s' fehlt", index, field)
		}
	}
	return nil
}

// idFromRequest liest die ID aus dem Request - entweder als Query-Parameter (?id=5)
// oder (Fallback) aus dem letzten Pfadsegment der URL (/api/applications/5).
// Gibt false zurück, wenn keine gültige Zahl gefunden wurde.
// (PHP hat "abc" stillschweigend zu 0 gemacht - hier gibt es stattdessen einen sauberen 400er.)
func idFromRequest(r *http.Request) (int64, bool) {
	if q := r.URL.Query().Get("id"); q != "" {
		id, err := strconv.ParseInt(q, 10, 64)
		return id, err == nil
	}
	path := strings.Trim(r.URL.Path, "/")
	last := path[strings.LastIndex(path, "/")+1:]
	id, err := strconv.ParseInt(last, 10, 64)
	return id, err == nil
}

// idOf liest die ID eines Eintrags als int64 (entspricht dem (int)-Cast in PHP).
// Akzeptiert Zahlen und numerische Strings; alles andere gilt als "keine ID".
func idOf(app Application) (int64, bool) {
	switch v := app["id"].(type) {
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, true
		}
		if f, err := v.Float64(); err == nil {
			return int64(f), true
		}
	case string:
		if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

// --- Datenspeicher (ersetzt openLockedFile / closeLockedFile / read- & writeApplicationsLocked) ---

// store kapselt die Datenhaltung: pro User-ID (aus dem "sub"-Claim des Tokens) eine eigene
// JSON-Datei im selben Verzeichnis. Statt flock() reicht in Go ein Mutex, weil ein einziger
// langlebiger Prozess alle Requests bedient (PHP startet dagegen pro Request einen neuen Prozess).
// Ein einziger Mutex für alle Nutzer ist bewusst einfach gehalten: bei sehr vielen gleichzeitigen
// Nutzern blockieren sich deren Requests gegenseitig kurz, auch wenn sie unterschiedliche Dateien
// betreffen. Für den Umfang dieser App ist das unkritisch; bei Bedarf ließe sich das später auf
// einen Mutex pro User-ID umstellen.
type store struct {
	mu  sync.RWMutex
	dir string
}

// userIDPattern lässt nur Zeichen zu, die in jedem Dateisystem unproblematisch sind.
// Zitadel-User-IDs sind normalerweise rein numerisch, das ist eine defensive Zusatzsicherung -
// falls doch mal exotische Zeichen im "sub"-Claim landen, verhindert das u.a. Path Traversal
// (z.B. eine User-ID wie "../../etc/passwd").
var userIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// pathFor liefert den Dateipfad für eine User-ID und prüft dabei deren Format.
func (s *store) pathFor(userID string) (string, error) {
	if !userIDPattern.MatchString(userID) {
		return "", fmt.Errorf("ungültige User-ID: %q", userID)
	}
	return filepath.Join(s.dir, "applications_"+userID+".json"), nil
}

// load liest die Datei einer User-ID. Der Aufrufer muss den Lock bereits halten.
// Fehlende oder leere Datei = leere Liste (z.B. beim allerersten Zugriff dieses Nutzers -
// die Datei wird dann erst beim nächsten Schreibvorgang tatsächlich angelegt).
// Kaputtes JSON ist dagegen ein Fehler: PHP hat das stillschweigend als leere Liste behandelt,
// womit der nächste Schreibvorgang alle Daten überschrieben hätte.
func (s *store) load(path string) ([]Application, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Application{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return []Application{}, nil
	}
	var apps []Application
	if err := decodeJSON(bytes.NewReader(data), &apps); err != nil {
		return nil, fmt.Errorf("%s enthält kein gültiges JSON: %w", path, err)
	}
	if apps == nil { // Datei enthielt "null"
		apps = []Application{}
	}
	return apps, nil
}

// save schreibt die komplette Liste einer User-ID als JSON. Der Aufrufer muss den Lock
// (exklusiv) halten. Es wird erst in eine temporäre Datei geschrieben und dann per Rename
// ersetzt: Rename ist atomar, d.h. selbst bei einem Absturz mitten im Schreiben bleibt die
// alte Datei intakt (PHP hat die Datei erst geleert und dann neu befüllt - dazwischen war
// sie kurz leer). Existiert die Datei noch nicht (erster Schreibzugriff dieses Nutzers),
// legt os.CreateTemp sie im selben Verzeichnis an, os.Rename benennt sie dann passend um.
func (s *store) save(path string, apps []Application) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)  // entspricht JSON_UNESCAPED_UNICODE: kein \u003c für "<" o.ä.
	enc.SetIndent("", "    ") // entspricht JSON_PRETTY_PRINT (4 Leerzeichen)
	if err := enc.Encode(apps); err != nil {
		return err
	}

	// Temp-Datei im selben Ordner, sonst funktioniert Rename nicht über Dateisystemgrenzen hinweg.
	tmp, err := os.CreateTemp(filepath.Dir(path), "applications-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // räumt bei Fehlern auf; nach erfolgreichem Rename ist der Name weg -> harmlos

	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // auf die Platte zwingen, bevor umbenannt wird
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// all gibt alle Bewerbungen einer User-ID zurück (Lesezugriff, mehrere GETs dürfen parallel laufen).
func (s *store) all(userID string) ([]Application, error) {
	path, err := s.pathFor(userID)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.load(path)
}

// replace ersetzt die komplette Liste einer User-ID (POST).
func (s *store) replace(userID string, apps []Application) error {
	path, err := s.pathFor(userID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save(path, apps)
}

// patch aktualisiert nur die übergebenen Felder eines Eintrags einer User-ID.
// Gibt false zurück, wenn die ID nicht existiert.
func (s *store) patch(userID string, id int64, updates Application) (bool, error) {
	path, err := s.pathFor(userID)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	apps, err := s.load(path)
	if err != nil {
		return false, err
	}
	for _, app := range apps {
		if appID, ok := idOf(app); !ok || appID != id {
			continue
		}
		// Map ist ein Referenztyp: Änderungen an app wirken direkt auf das Element im Slice.
		for _, field := range requiredFields {
			if field == "id" { // ID selbst darf nicht verändert werden
				continue
			}
			if v, has := updates[field]; has {
				app[field] = v
			}
		}
		return true, s.save(path, apps)
	}
	return false, nil
}

// remove löscht den Eintrag mit der ID bei einer User-ID.
// Gibt zurück, ob er existierte, und wie viele Einträge übrig sind.
func (s *store) remove(userID string, id int64) (remaining int, found bool, err error) {
	path, pathErr := s.pathFor(userID)
	if pathErr != nil {
		return 0, false, pathErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	apps, err := s.load(path)
	if err != nil {
		return 0, false, err
	}
	// Alle Einträge behalten, außer dem mit der gesuchten ID
	kept := make([]Application, 0, len(apps))
	for _, app := range apps {
		if appID, ok := idOf(app); ok && appID == id {
			continue
		}
		kept = append(kept, app)
	}
	// Wenn sich die Anzahl nicht geändert hat, gab es die ID gar nicht
	if len(kept) == len(apps) {
		return len(apps), false, nil
	}
	if err := s.save(path, kept); err != nil {
		return 0, false, err
	}
	return len(kept), true, nil
}

// --- HTTP-Handler ---

type server struct {
	store *store
}

// handleApplications verteilt nach HTTP-Methode (entspricht den if ($method === ...)-Blöcken in PHP).
func (s *server) handleApplications(w http.ResponseWriter, r *http.Request) {
	// Die Auth-Middleware (auth.go) hat den Request bereits geprüft, bevor er hierher kommt -
	// die User-ID muss also da sein. Fehlt sie trotzdem, ist das ein Programmierfehler
	// (Middleware falsch verdrahtet), kein Auth-Problem des Aufrufers -> 500, nicht 401.
	userID, ok := userIDFromContext(r.Context())
	if !ok {
		internalError(w, errors.New("keine User-ID im Request-Context (Auth-Middleware nicht aktiv?)"))
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, userID)
	case http.MethodPost:
		s.handlePost(w, r, userID)
	case http.MethodPatch:
		s.handlePatch(w, r, userID)
	case http.MethodDelete:
		s.handleDelete(w, r, userID)
	default:
		// Falls keine der obigen Methoden zutraf (z.B. PUT)
		w.Header().Set("Allow", "GET, POST, PATCH, DELETE, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, "Methode nicht erlaubt")
	}
}

// Interne Fehler (Dateisystem etc.) werden geloggt, aber nicht im Detail an den Client gegeben.
func internalError(w http.ResponseWriter, err error) {
	log.Printf("Interner Fehler: %v", err)
	writeError(w, http.StatusInternalServerError, "Interner Serverfehler")
}

// GET: alle Bewerbungen des eingeloggten Nutzers lesen
func (s *server) handleGet(w http.ResponseWriter, userID string) {
	apps, err := s.store.all(userID)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apps)
}

// POST: komplette Liste des eingeloggten Nutzers ersetzen
func (s *server) handlePost(w http.ResponseWriter, r *http.Request, userID string) {
	var entries []Application
	if !decodeBody(w, r, &entries, "Erwartet wird ein JSON-Array von Bewerbungs-Objekten") {
		return
	}
	if entries == nil { // Body war das JSON-Literal "null"
		writeError(w, http.StatusBadRequest, "Erwartet wird ein JSON-Array von Bewerbungen")
		return
	}
	// Jeden einzelnen Eintrag im Array validieren, bevor irgendwas gespeichert wird
	for i, entry := range entries {
		if entry == nil { // Element war "null"
			writeError(w, http.StatusBadRequest, fmt.Sprintf("Eintrag %d ist kein Objekt", i))
			return
		}
		if err := validateEntry(entry, i); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	if err := s.store.replace(userID, entries); err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "count": len(entries)})
}

// PATCH: einzelnen Eintrag des eingeloggten Nutzers teilweise aktualisieren
func (s *server) handlePatch(w http.ResponseWriter, r *http.Request, userID string) {
	id, ok := idFromRequest(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "ID fehlt oder ist ungültig (?id=... angeben)")
		return
	}

	// Body enthält nur die zu ändernden Felder, nicht den kompletten Eintrag
	const expected = "Erwartet wird ein JSON-Objekt mit den zu ändernden Feldern"
	var updates Application
	if !decodeBody(w, r, &updates, expected) {
		return
	}
	if updates == nil { // Body war "null"
		writeError(w, http.StatusBadRequest, expected)
		return
	}

	found, err := s.store.patch(userID, id, updates)
	if err != nil {
		internalError(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Eintrag mit id=%d nicht gefunden", id))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id})
}

// DELETE: einzelnen Eintrag des eingeloggten Nutzers löschen
func (s *server) handleDelete(w http.ResponseWriter, r *http.Request, userID string) {
	id, ok := idFromRequest(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "ID fehlt oder ist ungültig (?id=... angeben)")
		return
	}

	remaining, found, err := s.store.remove(userID, id)
	if err != nil {
		internalError(w, err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Eintrag mit id=%d nicht gefunden", id))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": id, "remaining": remaining})
}

// --- Middleware ---

// withCORS setzt die CORS- und Content-Type-Header für alle Antworten.
// Browser schicken vor "echten" Requests mit Authorization-Header oft einen OPTIONS-Preflight.
// Der braucht keine Auth-Prüfung, nur eine leere Erfolgsantwort.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		h.Set("Access-Control-Max-Age", "86400") // Preflight-Antwort 24h im Browser cachen
		h.Set("Content-Type", "application/json; charset=utf-8")
		h.Set("Cache-Control", "no-store") // API-Antworten (auch Fehler) sollen nie gecacht werden

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- Start ---

func main() {
	// Konfiguration über Umgebungsvariablen (siehe systemd-Unit), sinnvolle Standardwerte für lokale Tests.
	// 127.0.0.1 = nur lokal erreichbar (gedacht für den Betrieb hinter einem Reverse-Proxy).
	addr := env("ADDR", "127.0.0.1:8080")
	dataDir := env("DATA_DIR", "./data")

	// Entspricht ensureDataDir(): Ordner beim Start anlegen, falls er fehlt.
	// Die einzelnen Dateien pro User-ID (applications_<userID>.json) legt der Store
	// selbst beim ersten Schreibzugriff des jeweiligen Nutzers an.
	if err := os.MkdirAll(dataDir, 0o775); err != nil {
		log.Fatalf("Datenordner konnte nicht angelegt werden: %v", err)
	}

	srv := &server{store: &store{dir: dataDir}}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/applications", srv.handleApplications)
	mux.HandleFunc("/api/applications/", srv.handleApplications)    // erlaubt /api/applications/5
	mux.HandleFunc("/api/applications.php", srv.handleApplications) // Übergangslösung: alte URL aus dem PHP-Frontend, kann später weg
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "Nicht gefunden")
	})

	// Auth-Middleware (siehe auth.go): prüft JWT-Signatur, Issuer, Audience und - falls
	// ZITADEL_REQUIRED_ROLE gesetzt ist - die Zitadel-Projekt-Rolle. Reihenfolge wichtig:
	// nach CORS (Preflight braucht keinen Token), vor den eigentlichen Handlern.
	auth, err := newAuthConfig()
	if err != nil {
		log.Fatalf("Auth-Konfiguration ungültig: %v", err)
	}
	handler := withCORS(auth.withAuth(mux))

	// Timeouts schützen vor hängenden bzw. absichtlich langsamen Verbindungen.
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Sauberes Herunterfahren bei SIGTERM (systemctl stop/restart) bzw. Strg+C:
	// laufende Requests dürfen noch fertig werden, bevor der Prozess endet.
	shutdownDone := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("Fehler beim Herunterfahren: %v", err)
		}
		close(shutdownDone)
	}()

	log.Printf("API läuft auf %s, Datenordner: %s", addr, dataDir)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Server-Fehler: %v", err)
	}
	<-shutdownDone
	log.Println("Server beendet")
}
