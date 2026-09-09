package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	authsvc "agora/services/auth"
	cartsvc "agora/services/cart"
	catalogsvc "agora/services/catalog"
	orderssvc "agora/services/orders"
	"agora/shared/middleware"
	"agora/shared/models"
	"agora/shared/store"
)

// TemplateEngine owns the application's server-side HTML rendering.
// Templates are parsed for the requested page so that each page can define
// its own "content" block without colliding with other pages.
type TemplateEngine struct {
	templateDir string
}

// ExecuteTemplate renders a page using the common base layout.
//
// Each page is composed from:
//   - base.html: shared document structure
//   - page: page-specific content
//
// The page template is expected to define a template named "content".
// base.html is responsible for invoking that template.
func (te *TemplateEngine) ExecuteTemplate(
	w http.ResponseWriter,
	page string,
	data interface{},
) error {
	tmpl, err := template.New("base").
		Funcs(templateFuncs()).
		ParseFiles(
			te.templatePath("base.html"),
			te.templatePath(page),
		)
	if err != nil {
		log.Printf("template parse failed for %q: %v", page, err)
		http.Error(
			w,
			http.StatusText(http.StatusInternalServerError),
			http.StatusInternalServerError,
		)
		return err
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
		log.Printf("template execution failed for %q: %v", page, err)

		// At this point the response may already contain part of the page,
		// so avoid attempting to write another HTTP error response blindly.
		return err
	}

	return nil
}

// templatePath resolves a template name relative to the configured template
// directory. Keeping path construction here makes the rendering layer easier
// to change if templates are moved or embedded later.
func (te *TemplateEngine) templatePath(page string) string {
	return te.templateDir + "/" + page
}

// templateFuncs contains functions exposed to html/template.
//
// Keep template functions deterministic and presentation-oriented. Business
// logic should remain in services rather than being implemented in templates.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"mul": func(a, b float64) float64 {
			return a * b
		},

		"add": func(a, b float64) float64 {
			return a + b
		},

		"toFloat": func(i int) float64 {
			return float64(i)
		},

		"not": func(v interface{}) bool {
			if v == nil {
				return true
			}

			if b, ok := v.(bool); ok {
				return !b
			}

			if s, ok := v.(string); ok {
				return s == ""
			}

			return false
		},

		"countInStock": func(products []models.Product) int {
			count := 0

			for _, product := range products {
				if product.Stock > 0 {
					count++
				}
			}

			return count
		},

		"totalValue": func(products []models.Product) string {
			var total float64

			for _, product := range products {
				total += product.Price * float64(product.Stock)
			}

			return strconv.FormatFloat(total, 'f', 0, 64)
		},

		"year": func() int {
			return time.Now().Year()
		},
	}
}

// hashPassword provides the password hashing implementation used by the
// existing MVP.
//
// This preserves the application's current authentication behavior. For a
// production system, replace this with a password-specific KDF such as
// Argon2id or bcrypt and migrate existing password hashes accordingly.
func hashPassword(password string) string {
	h := sha256.New()
	h.Write([]byte("agora_salt_2026_" + password))

	return hex.EncodeToString(h.Sum(nil))
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Initialise the shared database/store before constructing services.
	db := store.Get()

	// The engine is intentionally lightweight. Templates are parsed when a
	// specific page is rendered rather than loading every template at startup.
	tmpl := &TemplateEngine{
		templateDir: "web/templates",
	}

	// Services encapsulate application/domain behavior. HTTP handlers should
	// delegate business operations to these services rather than accessing the
	// database directly.
	authService := authsvc.New(db)
	catalogService := catalogsvc.New(db)
	cartService := cartsvc.New(db)
	ordersService := orderssvc.New(db, cartService)

	// Seed catalog data required by the MVP.
	catalogService.SeedSampleData()

	// Create the development/demo buyer account.
	//
	// The underlying CreateUser implementation should ideally make this
	// idempotent so repeated application starts do not create duplicates.
	db.CreateUser(models.User{
		Name:         "Demo Buyer",
		Email:        "buyer@demo.ke",
		PasswordHash: hashPassword("demo123"),
		Role:         "buyer",
	})

	// Register application routes.
	mux := http.NewServeMux()

	authService.RegisterRoutes(mux, tmpl)
	catalogService.RegisterRoutes(mux, tmpl)
	cartService.RegisterRoutes(mux, tmpl)
	ordersService.RegisterRoutes(mux, tmpl)

	// Health endpoint intentionally remains simple and independent of template
	// rendering so it can be used by process supervisors and load balancers.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		fmt.Fprint(
			w,
			`{"status":"ok","service":"agora","version":"1.0.0"}`,
		)
	})

	// Apply authentication/session middleware once around the complete router.
	// The previous implementation constructed this middleware twice, which was
	// unnecessary and made the request flow harder to reason about.
	authHandler := middleware.Auth(db)(mux)

	// Keep request logging separate from authentication so middleware ordering
	// remains explicit and easy to modify.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)

		authHandler.ServeHTTP(w, r)
	})

	log.Printf("Agora running at http://localhost:%s", port)
	log.Printf("Demo buyer: buyer@demo.ke / demo123")
	log.Printf("Demo seller: seller@agora.ke / test")

	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}
