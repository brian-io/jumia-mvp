package orders

import (
	"agora/shared/middleware"
	"agora/shared/models"
	"agora/shared/store"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type CartService interface {
	GetCart(userID int) ([]models.CartItem, float64)
	ClearCart(userID int)
}

type Service struct {
	DB   *store.DB
	Cart CartService
}

func New(db *store.DB, cart CartService) *Service { return &Service{DB: db, Cart: cart} }

type Tmpl interface {
	ExecuteTemplate(http.ResponseWriter, string, interface{}) error
}

func (s *Service) RegisterRoutes(mux *http.ServeMux, tmpl Tmpl) {
	mux.HandleFunc("/checkout", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		userID, _ := middleware.GetUserID(r)
		items, total := s.Cart.GetCart(userID)

		if r.Method == http.MethodGet {
			tmpl.ExecuteTemplate(w, "checkout.html", map[string]interface{}{
				"Title": "Checkout", "Items": items, "Total": total,
				"LoggedIn": true, "UserName": middleware.GetUserName(r), "UserRole": middleware.GetUserRole(r),
			})
			return
		}

		r.ParseForm()
		address := r.FormValue("address")
		if address == "" || len(items) == 0 {
			tmpl.ExecuteTemplate(w, "checkout.html", map[string]interface{}{
				"Title": "Checkout", "Error": "Address required and cart must not be empty",
				"Items": items, "Total": total,
				"LoggedIn": true, "UserName": middleware.GetUserName(r), "UserRole": middleware.GetUserRole(r),
			})
			return
		}

		var orderItems []models.OrderItem
		for _, ci := range items {
			orderItems = append(orderItems, models.OrderItem{
				ProductID: ci.ProductID, Quantity: ci.Quantity, Price: ci.Product.Price,
				Product: ci.Product,
			})
		}

		order, err := s.DB.CreateOrder(models.Order{
			UserID: userID, Total: total, Status: "pending", Address: address,
		}, orderItems)
		if err != nil {
			tmpl.ExecuteTemplate(w, "checkout.html", map[string]interface{}{
				"Title": "Checkout", "Error": err.Error(),
				"Items": items, "Total": total,
				"LoggedIn": true, "UserName": middleware.GetUserName(r), "UserRole": middleware.GetUserRole(r),
			})
			return
		}
		s.Cart.ClearCart(userID)
		http.Redirect(w, r, fmt.Sprintf("/orders/%d?success=1", order.ID), http.StatusFound)
	}))

	mux.HandleFunc("/orders", middleware.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		userID, _ := middleware.GetUserID(r)
		orders := s.DB.GetOrders(userID)
		tmpl.ExecuteTemplate(w, "orders.html", map[string]interface{}{
			"Title": "My Orders", "Orders": orders,
			"LoggedIn": true, "UserName": middleware.GetUserName(r), "UserRole": middleware.GetUserRole(r),
		})
	}))

	mux.HandleFunc("/orders/", func(w http.ResponseWriter, r *http.Request) {
		userID, ok := middleware.GetUserID(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		parts := strings.Split(r.URL.Path, "/")
		orderID, _ := strconv.Atoi(parts[len(parts)-1])
		order, err := s.DB.GetOrder(orderID, userID)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		tmpl.ExecuteTemplate(w, "order_detail.html", map[string]interface{}{
			"Title":    fmt.Sprintf("Order #%d", order.ID),
			"Order":    order,
			"Success":  r.URL.Query().Get("success") == "1",
			"LoggedIn": true, "UserName": middleware.GetUserName(r), "UserRole": middleware.GetUserRole(r),
		})
	})
}
