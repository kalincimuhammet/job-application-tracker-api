// auth.go
//
// JWT-Prüfung gegen Zitadel Cloud - Go-Port von auth.php / checkAuth().
// Wichtig: In der Zitadel Console muss der "Auth Token Type" der Application
// explizit auf JWT gestellt sein, sonst gibt Zitadel opake Tokens aus, die
// sich hiermit NICHT prüfen lassen (siehe decisions-and-learnings: bekannter Stolperstein).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// Der Rollen-Claim, unter dem Zitadel Projekt-Rollen im Token ausliefert.
// Wert ist ein Objekt {"<rolle>": {"<orgId>": "<orgName>", ...}, ...}.
const zitadelRolesClaim = "urn:zitadel:iam:org:project:roles"

// authConfig bündelt die Auth-Konfiguration, damit sie nicht bei jedem Request
// erneut aus den Umgebungsvariablen gelesen werden muss.
type authConfig struct {
	issuer       string // z.B. https://deine-instanz-abc123.zitadel.cloud
	audience     string // Projekt-Resource-ID bzw. Client-ID aus Zitadel
	requiredRole string // Projekt-Rolle, die der Nutzer haben muss (leer = kein Rollen-Check)
	jwks         keyfunc.Keyfunc
}

// newAuthConfig liest die Konfiguration aus Umgebungsvariablen und lädt das JWKS
// (die öffentlichen Signaturschlüssel von Zitadel). Das JWKS wird von keyfunc
// automatisch im Hintergrund aktualisiert (Refresh-Intervall unten) - das entspricht
// dem "JWKS caching" aus der PHP-Version, nur eben eingebaut statt selbstgebaut.
func newAuthConfig() (*authConfig, error) {
	issuer := os.Getenv("ZITADEL_ISSUER")
	audience := os.Getenv("ZITADEL_AUDIENCE")
	requiredRole := os.Getenv("ZITADEL_REQUIRED_ROLE") // optional; leer lassen für "nur gültiges Token reicht"

	if issuer == "" || audience == "" {
		return nil, errors.New("ZITADEL_ISSUER und ZITADEL_AUDIENCE müssen gesetzt sein")
	}
	issuer = strings.TrimRight(issuer, "/")

	jwksURL := issuer + "/oauth/v2/keys"
	jwks, err := keyfunc.NewDefault([]string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("JWKS konnte nicht geladen werden (%s): %w", jwksURL, err)
	}

	if requiredRole == "" {
		log.Println("Warnung: ZITADEL_REQUIRED_ROLE ist nicht gesetzt - es wird nur ein gültiges Token geprüft, keine Rolle")
	}

	return &authConfig{
		issuer:       issuer,
		audience:     audience,
		requiredRole: requiredRole,
		jwks:         jwks,
	}, nil
}

// sendAuthError entspricht sendAuthError() aus auth.php: einheitliche 401/403-Antwort.
func sendAuthError(w http.ResponseWriter, status int, message string) {
	writeError(w, status, message)
}

// checkAuth prüft den Authorization-Header und gibt bei Erfolg die Token-Claims zurück.
// Bei Fehlern wird direkt die Fehlerantwort geschrieben; der Aufrufer erkennt das am ok=false
// und muss dann selbst mit return abbrechen (kein exit() wie in PHP).
func (a *authConfig) checkAuth(w http.ResponseWriter, r *http.Request) (jwt.MapClaims, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		sendAuthError(w, http.StatusUnauthorized, "Authorization-Header fehlt")
		return nil, false
	}
	tokenString, found := strings.CutPrefix(header, "Bearer ")
	if !found || tokenString == "" {
		sendAuthError(w, http.StatusUnauthorized, "Authorization-Header muss 'Bearer <token>' sein")
		return nil, false
	}

	token, err := jwt.Parse(
		tokenString,
		a.jwks.Keyfunc,
		jwt.WithValidMethods([]string{"RS256"}), // Zitadel signiert mit RS256; verhindert das "alg: none"-Angriffsmuster
		jwt.WithIssuer(a.issuer),
		jwt.WithAudience(a.audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		log.Printf("Token-Prüfung fehlgeschlagen: %v", err)
		sendAuthError(w, http.StatusUnauthorized, "Ungültiges oder abgelaufenes Token")
		return nil, false
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		sendAuthError(w, http.StatusUnauthorized, "Ungültiges Token")
		return nil, false
	}

	if a.requiredRole != "" && !hasRole(claims, a.requiredRole) {
		sendAuthError(w, http.StatusForbidden, "Fehlende Rolle: "+a.requiredRole)
		return nil, false
	}

	return claims, true
}

// hasRole prüft, ob der Rollen-Claim die gesuchte Rolle enthält.
// Der Claim hat die Form {"rolle-a": {...}, "rolle-b": {...}}, uns interessiert nur,
// ob der Schlüssel vorhanden ist - welche Organisation dahintersteht, ist hier nicht relevant.
func hasRole(claims jwt.MapClaims, role string) bool {
	rolesClaim, ok := claims[zitadelRolesClaim]
	if !ok {
		return false
	}
	roles, ok := rolesClaim.(map[string]any)
	if !ok {
		return false
	}
	_, has := roles[role]
	return has
}

// withAuth ist die Middleware-Variante von checkAuth() für den Einsatz in der Handler-Kette
// (nach withCORS, vor den eigentlichen Handlern - siehe TODO in main.go).
// Die Claims werden über den Request-Context an nachgelagerte Handler weitergereicht,
// z.B. um wie in PHP $tokenPayload (Username/E-Mail) auszulesen.
type contextKey string

const claimsContextKey contextKey = "zitadelClaims"

func (a *authConfig) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := a.checkAuth(w, r)
		if !ok {
			return // Antwort wurde bereits von checkAuth geschrieben
		}
		ctx := context.WithValue(r.Context(), claimsContextKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// claimsFromContext liest die Token-Claims aus dem Request-Context.
// Praktisch für Handler, die z.B. die E-Mail des eingeloggten Nutzers brauchen:
//
//	claims, _ := claimsFromContext(r.Context())
//	email, _ := claims["email"].(string)
func claimsFromContext(ctx context.Context) (jwt.MapClaims, bool) {
	claims, ok := ctx.Value(claimsContextKey).(jwt.MapClaims)
	return claims, ok
}

// userIDFromContext liest die User-ID aus dem "sub"-Claim (Zitadel: die eindeutige,
// unveränderliche Nutzer-ID). Jeder Handler, der pro Nutzer getrennte Daten braucht,
// ruft das auf, statt selbst im Claims-Objekt zu wühlen.
func userIDFromContext(ctx context.Context) (string, bool) {
	claims, ok := claimsFromContext(ctx)
	if !ok {
		return "", false
	}
	sub, ok := claims["sub"].(string)
	if !ok || sub == "" {
		return "", false
	}
	return sub, true
}
