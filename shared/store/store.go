// Package store provides a zero-dependency JSON-backed data store.
// Perfect for MVP/launch - can be swapped for Postgres/SQLite later.
package store

import (
	"encoding/json"
	"fmt"
	"jumia-mvp/shared/models"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type DB struct {
	mu       sync.RWMutex
	path     string
	data     *Data
}

type Data struct {
	Users      []models.User      `json:"users"`
	Products   []models.Product   `json:"products"`
	CartItems  []models.CartItem  `json:"cart_items"`
	Orders     []models.Order     `json:"orders"`
	OrderItems []models.OrderItem `json:"order_items"`
	Sessions   []models.Session   `json:"sessions"`
	NextIDs    map[string]int     `json:"next_ids"`
}

var instance *DB
var once sync.Once

func Get() *DB {
	once.Do(func() {
		path := os.Getenv("DB_PATH")
		if path == "" {
			path = "./jumia.json"
		}
		instance = &DB{path: path}
		instance.load()
	})
	return instance
}

func (db *DB) load() {
	db.data = &Data{NextIDs: map[string]int{
		"users": 1, "products": 1, "cart_items": 1,
		"orders": 1, "order_items": 1,
	}}
	b, err := os.ReadFile(db.path)
	if err != nil {
		return // fresh start
	}
	json.Unmarshal(b, db.data)
}

func (db *DB) save() {
	b, _ := json.MarshalIndent(db.data, "", "  ")
	os.WriteFile(db.path, b, 0644)
}

func (db *DB) nextID(table string) int {
	id := db.data.NextIDs[table]
	db.data.NextIDs[table] = id + 1
	return id
}

// ── Users ──────────────────────────────────────────────

func (db *DB) CreateUser(u models.User) (*models.User, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, existing := range db.data.Users {
		if strings.EqualFold(existing.Email, u.Email) {
			return nil, fmt.Errorf("email already registered")
		}
	}
	u.ID = db.nextID("users")
	u.CreatedAt = time.Now()
	db.data.Users = append(db.data.Users, u)
	db.save()
	return &u, nil
}

func (db *DB) GetUserByEmailAndHash(email, hash string) (*models.User, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	for _, u := range db.data.Users {
		if strings.EqualFold(u.Email, email) && u.PasswordHash == hash {
			return &u, nil
		}
	}
	return nil, fmt.Errorf("invalid credentials")
}

func (db *DB) GetUserByID(id int) (*models.User, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	for _, u := range db.data.Users {
		if u.ID == id {
			return &u, nil
		}
	}
	return nil, fmt.Errorf("user not found")
}

// ── Sessions ──────────────────────────────────────────

func (db *DB) CreateSession(s models.Session) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.data.Sessions = append(db.data.Sessions, s)
	db.save()
	return nil
}

func (db *DB) GetSession(id string) (*models.Session, *models.User, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	for _, s := range db.data.Sessions {
		if s.ID == id && s.ExpiresAt.After(time.Now()) {
			for _, u := range db.data.Users {
				if u.ID == s.UserID {
					return &s, &u, nil
				}
			}
		}
	}
	return nil, nil, fmt.Errorf("invalid session")
}

func (db *DB) DeleteSession(id string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var filtered []models.Session
	for _, s := range db.data.Sessions {
		if s.ID != id {
			filtered = append(filtered, s)
		}
	}
	db.data.Sessions = filtered
	db.save()
}

// ── Products ──────────────────────────────────────────

func (db *DB) CreateProduct(p models.Product) (*models.Product, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	p.ID = db.nextID("products")
	p.CreatedAt = time.Now()
	db.data.Products = append(db.data.Products, p)
	db.save()
	return &p, nil
}

func (db *DB) GetProducts(category, search string) []models.Product {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var result []models.Product
	for _, p := range db.data.Products {
		if category != "" && p.Category != category {
			continue
		}
		if search != "" {
			s := strings.ToLower(search)
			if !strings.Contains(strings.ToLower(p.Name), s) &&
				!strings.Contains(strings.ToLower(p.Description), s) {
				continue
			}
		}
		result = append(result, p)
	}
	// Sort newest first
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

func (db *DB) GetProduct(id int) (*models.Product, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	for _, p := range db.data.Products {
		if p.ID == id {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("product not found")
}

func (db *DB) GetProductsBySellerID(sellerID int) []models.Product {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var result []models.Product
	for _, p := range db.data.Products {
		if p.SellerID == sellerID {
			result = append(result, p)
		}
	}
	return result
}

func (db *DB) DeleteProduct(id, sellerID int) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var filtered []models.Product
	for _, p := range db.data.Products {
		if !(p.ID == id && p.SellerID == sellerID) {
			filtered = append(filtered, p)
		}
	}
	db.data.Products = filtered
	db.save()
}

func (db *DB) GetCategories() []string {
	db.mu.RLock()
	defer db.mu.RUnlock()
	seen := map[string]bool{}
	var cats []string
	for _, p := range db.data.Products {
		if p.Category != "" && !seen[p.Category] {
			seen[p.Category] = true
			cats = append(cats, p.Category)
		}
	}
	sort.Strings(cats)
	return cats
}

func (db *DB) UpdateProductStock(productID, delta int) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for i, p := range db.data.Products {
		if p.ID == productID {
			db.data.Products[i].Stock += delta
			break
		}
	}
	db.save()
}

// ── Cart ──────────────────────────────────────────────

func (db *DB) GetCartItems(userID int) []models.CartItem {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var items []models.CartItem
	for _, ci := range db.data.CartItems {
		if ci.UserID == userID {
			// Attach product
			for _, p := range db.data.Products {
				if p.ID == ci.ProductID {
					ci.Product = p
					break
				}
			}
			items = append(items, ci)
		}
	}
	return items
}

func (db *DB) AddToCart(userID, productID, qty int) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	// Check stock
	var stock int
	for _, p := range db.data.Products {
		if p.ID == productID {
			stock = p.Stock
			break
		}
	}
	if stock < qty {
		return fmt.Errorf("insufficient stock")
	}
	// Update existing or add
	for i, ci := range db.data.CartItems {
		if ci.UserID == userID && ci.ProductID == productID {
			db.data.CartItems[i].Quantity += qty
			db.save()
			return nil
		}
	}
	db.data.CartItems = append(db.data.CartItems, models.CartItem{
		ID: db.nextID("cart_items"), UserID: userID, ProductID: productID, Quantity: qty,
	})
	db.save()
	return nil
}

func (db *DB) UpdateCartItem(userID, productID, qty int) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if qty <= 0 {
		var filtered []models.CartItem
		for _, ci := range db.data.CartItems {
			if !(ci.UserID == userID && ci.ProductID == productID) {
				filtered = append(filtered, ci)
			}
		}
		db.data.CartItems = filtered
	} else {
		for i, ci := range db.data.CartItems {
			if ci.UserID == userID && ci.ProductID == productID {
				db.data.CartItems[i].Quantity = qty
				break
			}
		}
	}
	db.save()
}

func (db *DB) RemoveCartItem(userID, productID int) {
	db.UpdateCartItem(userID, productID, 0)
}

func (db *DB) ClearCart(userID int) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var filtered []models.CartItem
	for _, ci := range db.data.CartItems {
		if ci.UserID != userID {
			filtered = append(filtered, ci)
		}
	}
	db.data.CartItems = filtered
	db.save()
}

// ── Orders ────────────────────────────────────────────

func (db *DB) CreateOrder(o models.Order, items []models.OrderItem) (*models.Order, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	o.ID = db.nextID("orders")
	o.CreatedAt = time.Now()

	for i := range items {
		items[i].ID = db.nextID("order_items")
		items[i].OrderID = o.ID
		db.data.OrderItems = append(db.data.OrderItems, items[i])
		// Reduce stock
		for j, p := range db.data.Products {
			if p.ID == items[i].ProductID {
				db.data.Products[j].Stock -= items[i].Quantity
				break
			}
		}
	}
	o.Items = items
	db.data.Orders = append(db.data.Orders, o)
	db.save()
	return &o, nil
}

func (db *DB) GetOrders(userID int) []models.Order {
	db.mu.RLock()
	defer db.mu.RUnlock()
	var result []models.Order
	for _, o := range db.data.Orders {
		if o.UserID == userID {
			// Attach items
			for _, oi := range db.data.OrderItems {
				if oi.OrderID == o.ID {
					for _, p := range db.data.Products {
						if p.ID == oi.ProductID {
							oi.Product = p
							break
						}
					}
					o.Items = append(o.Items, oi)
				}
			}
			result = append(result, o)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result
}

func (db *DB) GetOrder(orderID, userID int) (*models.Order, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	for _, o := range db.data.Orders {
		if o.ID == orderID && o.UserID == userID {
			for _, oi := range db.data.OrderItems {
				if oi.OrderID == o.ID {
					for _, p := range db.data.Products {
						if p.ID == oi.ProductID {
							oi.Product = p
							break
						}
					}
					o.Items = append(o.Items, oi)
				}
			}
			return &o, nil
		}
	}
	return nil, fmt.Errorf("order not found")
}
