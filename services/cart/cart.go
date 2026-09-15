package cart

import (
	"agora/shared/csrf"
	"agora/shared/middleware"
	"agora/shared/models"
	"agora/shared/store"
	"net/http"
	"strconv"
	"strings"
)

type Service struct {
	DB *store.DB
}

func New(db *store.DB) *Service {
	return &Service{DB: db}
}

func (s *Service) GetCart(userID int) ([]models.CartItem, float64) {
	items := s.DB.GetCartItems(userID)

	total := 0.0

	for _, ci := range items {
		total += ci.Product.Price * float64(ci.Quantity)
	}

	return items, total
}

func (s *Service) ClearCart(userID int) {
	s.DB.ClearCart(userID)
}

type Tmpl interface {
	ExecuteTemplate(http.ResponseWriter, string, interface{}) error
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, tmpl Tmpl) {
	mux.HandleFunc("/cart", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		userID, _ := middleware.GetUserID(r)

		items, total := s.GetCart(userID)

		token, err := csrf.IssueCookie(w, r)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		tmpl.ExecuteTemplate(w, "cart.html", map[string]interface{}{
			"Title":     "My Cart",
			"Items":     items,
			"Total":     total,
			"CSRFToken": token,
			"LoggedIn":  true,
			"UserName":  middleware.GetUserName(r),
			"UserRole":  middleware.GetUserRole(r),
		})
	}))

	mux.HandleFunc("/cart/add", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := csrf.Verify(r); err != nil {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		productID, err := strconv.Atoi(r.FormValue("product_id"))
		if err != nil || productID <= 0 {
			http.Error(w, "invalid product", http.StatusBadRequest)
			return
		}

		quantity, err := strconv.Atoi(r.FormValue("quantity"))
		if err != nil || quantity <= 0 {
			quantity = 1
		}

		userID, _ := middleware.GetUserID(r)

		s.DB.AddToCart(userID, productID, quantity)

		redirect := r.FormValue("redirect")
		if redirect == "" {
			redirect = "/cart"
		}

		http.Redirect(w, r, redirect, http.StatusFound)
	}))

	mux.HandleFunc("/cart/update", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := csrf.Verify(r); err != nil {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}

		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		productID, err := strconv.Atoi(r.FormValue("product_id"))
		if err != nil || productID <= 0 {
			http.Error(w, "invalid product", http.StatusBadRequest)
			return
		}

		quantity, err := strconv.Atoi(r.FormValue("quantity"))
		if err != nil || quantity < 0 {
			http.Error(w, "invalid quantity", http.StatusBadRequest)
			return
		}

		userID, _ := middleware.GetUserID(r)

		s.DB.UpdateCartItem(userID, productID, quantity)

		http.Redirect(w, r, "/cart", http.StatusFound)
	}))

	mux.HandleFunc("/cart/remove/", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		if err := csrf.Verify(r); err != nil {
			http.Error(w, "invalid CSRF token", http.StatusForbidden)
			return
		}

		parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")

		if len(parts) == 0 {
			http.Error(w, "invalid product", http.StatusBadRequest)
			return
		}

		productID, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil || productID <= 0 {
			http.Error(w, "invalid product", http.StatusBadRequest)
			return
		}

		userID, _ := middleware.GetUserID(r)

		s.DB.RemoveCartItem(userID, productID)

		http.Redirect(w, r, "/cart", http.StatusFound)
	}))
}