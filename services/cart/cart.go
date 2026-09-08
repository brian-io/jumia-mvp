package cart

import (
	"jumia-mvp/shared/middleware"
	"jumia-mvp/shared/models"
	"jumia-mvp/shared/store"
	"net/http"
	"strconv"
	"strings"
)

type Service struct{ DB *store.DB }

func New(db *store.DB) *Service { return &Service{DB: db} }

func (s *Service) GetCart(userID int) ([]models.CartItem, float64) {
	items := s.DB.GetCartItems(userID)
	total := 0.0
	for _, ci := range items { total += ci.Product.Price * float64(ci.Quantity) }
	return items, total
}

func (s *Service) ClearCart(userID int) { s.DB.ClearCart(userID) }

type Tmpl interface { ExecuteTemplate(http.ResponseWriter, string, interface{}) error }

func (s *Service) RegisterRoutes(mux *http.ServeMux, tmpl Tmpl) {
	mux.HandleFunc("/cart", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		userID, _ := middleware.GetUserID(r)
		items, total := s.GetCart(userID)
		tmpl.ExecuteTemplate(w, "cart.html", map[string]interface{}{
			"Title": "My Cart", "Items": items, "Total": total,
			"LoggedIn": true, "UserName": middleware.GetUserName(r), "UserRole": middleware.GetUserRole(r),
		})
	}))

	mux.HandleFunc("/cart/add", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		productID, _ := strconv.Atoi(r.FormValue("product_id"))
		quantity, _ := strconv.Atoi(r.FormValue("quantity"))
		if quantity == 0 { quantity = 1 }
		userID, _ := middleware.GetUserID(r)
		s.DB.AddToCart(userID, productID, quantity)
		redirect := r.FormValue("redirect")
		if redirect == "" { redirect = "/cart" }
		http.Redirect(w, r, redirect, http.StatusFound)
	}))

	mux.HandleFunc("/cart/update", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		productID, _ := strconv.Atoi(r.FormValue("product_id"))
		quantity, _ := strconv.Atoi(r.FormValue("quantity"))
		userID, _ := middleware.GetUserID(r)
		s.DB.UpdateCartItem(userID, productID, quantity)
		http.Redirect(w, r, "/cart", http.StatusFound)
	}))

	mux.HandleFunc("/cart/remove/", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := middleware.GetUserID(r)
		if !ok { http.Redirect(w, r, "/login", http.StatusFound); return }
		parts := strings.Split(r.URL.Path, "/")
		productID, _ := strconv.Atoi(parts[len(parts)-1])
		s.DB.RemoveCartItem(userID, productID)
		http.Redirect(w, r, "/cart", http.StatusFound)
	})
}
