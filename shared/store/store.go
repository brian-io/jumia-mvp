// Package store is a PostgreSQL-backed data store using plain relational
// columns only — no JSON columns, no marshaling on the hot path. Every
// exported type and method signature matches the original store, so
// calling code does not need to change.
package store

import (
	"agora/shared/models"
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

type DB struct {
	conn *sql.DB
}

var instance *DB
var once sync.Once

// Get returns the process-wide singleton DB, connecting on first use.
// Connection details come from DATABASE_URL if set, otherwise from the
// discrete PGHOST/PGPORT/PGUSER/PGPASSWORD/PGDATABASE/PGSSLMODE vars.
func Get() *DB {
	once.Do(func() {
		conn, err := sql.Open("postgres", dsnFromEnv())
		if err != nil {
			panic(fmt.Sprintf("store: failed to open postgres connection: %v", err))
		}
		conn.SetMaxOpenConns(20)
		conn.SetMaxIdleConns(5)
		conn.SetConnMaxLifetime(30 * time.Minute)

		if err := conn.Ping(); err != nil {
			panic(fmt.Sprintf("store: failed to connect to postgres: %v", err))
		}

		instance = &DB{conn: conn}
		if err := instance.ensureSchema(); err != nil {
			panic(fmt.Sprintf("store: failed to run schema migration: %v", err))
		}
	})
	return instance
}

func dsnFromEnv() string {
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url
	}
	host := getenvDefault("PGHOST", "localhost")
	port := getenvDefault("PGPORT", "5432")
	user := getenvDefault("PGUSER", "postgres")
	pass := os.Getenv("PGPASSWORD")
	name := getenvDefault("PGDATABASE", "agora")
	sslmode := getenvDefault("PGSSLMODE", "disable")
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		host, port, user, pass, name, sslmode)
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, so scan helpers
// work for single-row lookups and multi-row iteration alike.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func (db *DB) ensureSchema() error {
	_, err := db.conn.Exec(`
CREATE TABLE IF NOT EXISTS users (
	id            SERIAL PRIMARY KEY,
	name          TEXT NOT NULL DEFAULT '',
	email         TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	role          TEXT NOT NULL DEFAULT '',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS users_email_lower_idx ON users (LOWER(email));

CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions (user_id);

CREATE TABLE IF NOT EXISTS products (
	id          SERIAL PRIMARY KEY,
	name        TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	price       NUMERIC(12,2) NOT NULL DEFAULT 0,
	stock       INTEGER NOT NULL DEFAULT 0,
	category    TEXT NOT NULL DEFAULT '',
	image_url   TEXT NOT NULL DEFAULT '',
	seller_id   INTEGER NOT NULL,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS products_category_idx ON products (category);
CREATE INDEX IF NOT EXISTS products_seller_id_idx ON products (seller_id);

CREATE TABLE IF NOT EXISTS cart_items (
	id         SERIAL PRIMARY KEY,
	user_id    INTEGER NOT NULL,
	product_id INTEGER NOT NULL,
	quantity   INTEGER NOT NULL,
	UNIQUE (user_id, product_id)
);
CREATE INDEX IF NOT EXISTS cart_items_user_id_idx ON cart_items (user_id);

CREATE TABLE IF NOT EXISTS orders (
	id         SERIAL PRIMARY KEY,
	user_id    INTEGER NOT NULL,
	total      NUMERIC(12,2) NOT NULL DEFAULT 0,
	status     TEXT NOT NULL DEFAULT 'pending',
	address    TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS orders_user_id_idx ON orders (user_id);

CREATE TABLE IF NOT EXISTS order_items (
	id         SERIAL PRIMARY KEY,
	order_id   INTEGER NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
	product_id INTEGER NOT NULL,
	quantity   INTEGER NOT NULL,
	price      NUMERIC(12,2) NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS order_items_order_id_idx ON order_items (order_id);

-- M-Pesa STK Push payment attempts, one row per attempt, linked to an order.
CREATE TABLE IF NOT EXISTS payments (
	id                   SERIAL PRIMARY KEY,
	order_id             INTEGER NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
	phone                TEXT NOT NULL,
	amount               NUMERIC(12,2) NOT NULL,
	merchant_request_id  TEXT NOT NULL DEFAULT '',
	checkout_request_id  TEXT NOT NULL DEFAULT '',
	mpesa_receipt        TEXT NOT NULL DEFAULT '',
	status               TEXT NOT NULL DEFAULT 'pending',
	result_desc          TEXT NOT NULL DEFAULT '',
	created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS payments_checkout_request_id_idx
	ON payments (checkout_request_id) WHERE checkout_request_id <> '';
CREATE INDEX IF NOT EXISTS payments_order_id_idx ON payments (order_id);
`)
	return err
}

// withTx runs fn inside a transaction, committing on success and rolling
// back if fn returns an error or panics.
func (db *DB) withTx(fn func(tx *sql.Tx) error) (err error) {
	tx, err := db.conn.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		} else if err != nil {
			tx.Rollback()
		} else {
			err = tx.Commit()
		}
	}()
	err = fn(tx)
	return err
}

// ── Users ──────────────────────────────────────────────

func scanUser(row rowScanner) (*models.User, error) {
	var u models.User
	if err := row.Scan(&u.ID, &u.Name, &u.Email, &u.PasswordHash, &u.Role, &u.CreatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

const userCols = `id, name, email, password_hash, role, created_at`

func (db *DB) CreateUser(u models.User) (*models.User, error) {
	var exists bool
	if err := db.conn.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM users WHERE LOWER(email) = LOWER($1))`, u.Email,
	).Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("email already registered")
	}

	err := db.conn.QueryRow(
		`INSERT INTO users (name, email, password_hash, role, created_at)
		 VALUES ($1, $2, $3, $4, now())
		 RETURNING id, created_at`,
		u.Name, u.Email, u.PasswordHash, u.Role,
	).Scan(&u.ID, &u.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return nil, fmt.Errorf("email already registered")
		}
		return nil, err
	}
	return &u, nil
}

func (db *DB) GetUserByEmailAndHash(email, hash string) (*models.User, error) {
	row := db.conn.QueryRow(
		`SELECT `+userCols+` FROM users WHERE LOWER(email) = LOWER($1) AND password_hash = $2`,
		email, hash,
	)
	u, err := scanUser(row)
	if err != nil {
		return nil, fmt.Errorf("invalid credentials")
	}
	return u, nil
}

func (db *DB) GetUserByID(id int) (*models.User, error) {
	row := db.conn.QueryRow(`SELECT `+userCols+` FROM users WHERE id = $1`, id)
	u, err := scanUser(row)
	if err != nil {
		return nil, fmt.Errorf("user not found")
	}
	return u, nil
}

// ── Sessions ──────────────────────────────────────────

func (db *DB) CreateSession(s models.Session) error {
	_, err := db.conn.Exec(
		`INSERT INTO sessions (id, user_id, expires_at) VALUES ($1, $2, $3)
		 ON CONFLICT (id) DO UPDATE SET user_id = $2, expires_at = $3`,
		s.ID, s.UserID, s.ExpiresAt,
	)
	return err
}

func (db *DB) GetSession(id string) (*models.Session, *models.User, error) {
	row := db.conn.QueryRow(
		`SELECT s.id, s.user_id, s.expires_at,
		        u.id, u.name, u.email, u.password_hash, u.role, u.created_at
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.id = $1 AND s.expires_at > now()`,
		id,
	)
	var s models.Session
	var u models.User
	err := row.Scan(&s.ID, &s.UserID, &s.ExpiresAt, &u.ID, &u.Name, &u.Email, &u.PasswordHash, &u.Role, &u.CreatedAt)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid session")
	}
	return &s, &u, nil
}

func (db *DB) DeleteSession(id string) {
	db.conn.Exec(`DELETE FROM sessions WHERE id = $1`, id)
}

// ── Products ──────────────────────────────────────────

const productCols = `id, name, description, price, stock, category, image_url, seller_id, created_at`

func scanProduct(row rowScanner) (models.Product, error) {
	var p models.Product
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Stock, &p.Category, &p.ImageURL, &p.SellerID, &p.CreatedAt)
	return p, err
}

func (db *DB) CreateProduct(p models.Product) (*models.Product, error) {
	err := db.conn.QueryRow(
		`INSERT INTO products (name, description, price, stock, category, image_url, seller_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		 RETURNING id, created_at`,
		p.Name, p.Description, p.Price, p.Stock, p.Category, p.ImageURL, p.SellerID,
	).Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (db *DB) GetProducts(category, search string) []models.Product {
	query := `SELECT ` + productCols + ` FROM products WHERE 1=1`
	var args []interface{}
	n := 1
	if category != "" {
		query += fmt.Sprintf(" AND category = $%d", n)
		args = append(args, category)
		n++
	}
	if search != "" {
		query += fmt.Sprintf(" AND (name ILIKE $%d OR description ILIKE $%d)", n, n)
		args = append(args, "%"+search+"%")
		n++
	}
	query += " ORDER BY created_at DESC"

	rows, err := db.conn.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []models.Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			continue
		}
		result = append(result, p)
	}
	return result
}

func (db *DB) GetProduct(id int) (*models.Product, error) {
	row := db.conn.QueryRow(`SELECT `+productCols+` FROM products WHERE id = $1`, id)
	p, err := scanProduct(row)
	if err != nil {
		return nil, fmt.Errorf("product not found")
	}
	return &p, nil
}

func (db *DB) GetProductsBySellerID(sellerID int) []models.Product {
	rows, err := db.conn.Query(`SELECT `+productCols+` FROM products WHERE seller_id = $1`, sellerID)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []models.Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			continue
		}
		result = append(result, p)
	}
	return result
}

func (db *DB) DeleteProduct(id, sellerID int) {
	db.conn.Exec(`DELETE FROM products WHERE id = $1 AND seller_id = $2`, id, sellerID)
}

func (db *DB) GetCategories() []string {
	rows, err := db.conn.Query(`SELECT DISTINCT category FROM products WHERE category <> '' ORDER BY category`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var cats []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			continue
		}
		cats = append(cats, c)
	}
	return cats
}

// UpdateProductStock is a single atomic UPDATE now that there's no JSON
// copy of the row to keep in sync — no read-modify-write needed.
func (db *DB) UpdateProductStock(productID, delta int) {
	db.conn.Exec(`UPDATE products SET stock = stock + $1 WHERE id = $2`, delta, productID)
}

// ── Cart ──────────────────────────────────────────────

func (db *DB) GetCartItems(userID int) []models.CartItem {
	rows, err := db.conn.Query(
		`SELECT ci.id, ci.user_id, ci.product_id, ci.quantity, `+productCols+`
		 FROM cart_items ci JOIN products p ON p.id = ci.product_id
		 WHERE ci.user_id = $1`,
		userID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var items []models.CartItem
	for rows.Next() {
		var ci models.CartItem
		err := rows.Scan(
			&ci.ID, &ci.UserID, &ci.ProductID, &ci.Quantity,
			&ci.Product.ID, &ci.Product.Name, &ci.Product.Description, &ci.Product.Price,
			&ci.Product.Stock, &ci.Product.Category, &ci.Product.ImageURL, &ci.Product.SellerID, &ci.Product.CreatedAt,
		)
		if err != nil {
			continue
		}
		items = append(items, ci)
	}
	return items
}

func (db *DB) AddToCart(userID, productID, qty int) error {
	return db.withTx(func(tx *sql.Tx) error {
		var stock int
		if err := tx.QueryRow(`SELECT stock FROM products WHERE id = $1 FOR UPDATE`, productID).Scan(&stock); err != nil {
			return fmt.Errorf("product not found")
		}
		if stock < qty {
			return fmt.Errorf("insufficient stock")
		}
		_, err := tx.Exec(
			`INSERT INTO cart_items (user_id, product_id, quantity)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (user_id, product_id)
			 DO UPDATE SET quantity = cart_items.quantity + EXCLUDED.quantity`,
			userID, productID, qty,
		)
		return err
	})
}

func (db *DB) UpdateCartItem(userID, productID, qty int) {
	if qty <= 0 {
		db.conn.Exec(`DELETE FROM cart_items WHERE user_id = $1 AND product_id = $2`, userID, productID)
		return
	}
	// No-ops if the row doesn't exist, matching the original store's behaviour.
	db.conn.Exec(`UPDATE cart_items SET quantity = $1 WHERE user_id = $2 AND product_id = $3`, qty, userID, productID)
}

func (db *DB) RemoveCartItem(userID, productID int) {
	db.UpdateCartItem(userID, productID, 0)
}

func (db *DB) ClearCart(userID int) {
	db.conn.Exec(`DELETE FROM cart_items WHERE user_id = $1`, userID)
}

// ── Orders ────────────────────────────────────────────

func (db *DB) CreateOrder(o models.Order, items []models.OrderItem) (*models.Order, error) {
	err := db.withTx(func(tx *sql.Tx) error {
		if err := tx.QueryRow(
			`INSERT INTO orders (user_id, total, status, address, created_at)
			 VALUES ($1, $2, $3, $4, now())
			 RETURNING id, created_at`,
			o.UserID, o.Total, o.Status, o.Address,
		).Scan(&o.ID, &o.CreatedAt); err != nil {
			return err
		}

		for i := range items {
			items[i].OrderID = o.ID
			if err := tx.QueryRow(
				`INSERT INTO order_items (order_id, product_id, quantity, price)
				 VALUES ($1, $2, $3, $4) RETURNING id`,
				items[i].OrderID, items[i].ProductID, items[i].Quantity, items[i].Price,
			).Scan(&items[i].ID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE products SET stock = stock - $1 WHERE id = $2`, items[i].Quantity, items[i].ProductID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	o.Items = items
	return &o, nil
}

// attachOrderItems fills in o.Items with a single joined query, rather
// than one query per item, so listing orders stays cheap.
func (db *DB) attachOrderItems(o *models.Order) {
	rows, err := db.conn.Query(
		`SELECT oi.id, oi.order_id, oi.product_id, oi.quantity, oi.price, `+productCols+`
		 FROM order_items oi JOIN products p ON p.id = oi.product_id
		 WHERE oi.order_id = $1`,
		o.ID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	o.Items = nil
	for rows.Next() {
		var oi models.OrderItem
		err := rows.Scan(
			&oi.ID, &oi.OrderID, &oi.ProductID, &oi.Quantity, &oi.Price,
			&oi.Product.ID, &oi.Product.Name, &oi.Product.Description, &oi.Product.Price,
			&oi.Product.Stock, &oi.Product.Category, &oi.Product.ImageURL, &oi.Product.SellerID, &oi.Product.CreatedAt,
		)
		if err != nil {
			continue
		}
		o.Items = append(o.Items, oi)
	}
}

func (db *DB) GetOrders(userID int) []models.Order {
	rows, err := db.conn.Query(
		`SELECT id, user_id, total, status, address, created_at
		 FROM orders WHERE user_id = $1 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []models.Order
	for rows.Next() {
		var o models.Order
		if err := rows.Scan(&o.ID, &o.UserID, &o.Total, &o.Status, &o.Address, &o.CreatedAt); err != nil {
			continue
		}
		db.attachOrderItems(&o)
		result = append(result, o)
	}
	return result
}

func (db *DB) GetOrder(orderID, userID int) (*models.Order, error) {
	var o models.Order
	err := db.conn.QueryRow(
		`SELECT id, user_id, total, status, address, created_at
		 FROM orders WHERE id = $1 AND user_id = $2`,
		orderID, userID,
	).Scan(&o.ID, &o.UserID, &o.Total, &o.Status, &o.Address, &o.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("order not found")
	}
	db.attachOrderItems(&o)
	return &o, nil
}

// UpdateOrderStatus sets an order's status directly — e.g. "shipped" or
// "cancelled". Prefer UpdatePaymentResult below when the change comes
// from a payment callback, since that also keeps the payments row synced.
func (db *DB) UpdateOrderStatus(orderID int, status string) error {
	_, err := db.conn.Exec(`UPDATE orders SET status = $1 WHERE id = $2`, status, orderID)
	return err
}

// ── Payments (M-Pesa) ──────────────────────────────────

const paymentCols = `id, order_id, phone, amount, merchant_request_id, checkout_request_id, mpesa_receipt, status, result_desc, created_at, updated_at`

func scanPayment(row rowScanner) (*models.Payment, error) {
	var p models.Payment
	err := row.Scan(
		&p.ID, &p.OrderID, &p.Phone, &p.Amount, &p.MerchantRequestID, &p.CheckoutRequestID,
		&p.MpesaReceipt, &p.Status, &p.ResultDesc, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePayment records a new STK Push attempt (status "pending") right
// after Safaricom accepts the push request.
func (db *DB) CreatePayment(p models.Payment) (*models.Payment, error) {
	if p.Status == "" {
		p.Status = "pending"
	}
	err := db.conn.QueryRow(
		`INSERT INTO payments (order_id, phone, amount, merchant_request_id, checkout_request_id, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, now(), now())
		 RETURNING id, created_at, updated_at`,
		p.OrderID, p.Phone, p.Amount, p.MerchantRequestID, p.CheckoutRequestID, p.Status,
	).Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (db *DB) GetPaymentByCheckoutRequestID(checkoutRequestID string) (*models.Payment, error) {
	row := db.conn.QueryRow(`SELECT `+paymentCols+` FROM payments WHERE checkout_request_id = $1`, checkoutRequestID)
	p, err := scanPayment(row)
	if err != nil {
		return nil, fmt.Errorf("payment not found")
	}
	return p, nil
}

func (db *DB) GetPaymentByOrderID(orderID int) (*models.Payment, error) {
	row := db.conn.QueryRow(
		`SELECT `+paymentCols+` FROM payments WHERE order_id = $1 ORDER BY created_at DESC LIMIT 1`,
		orderID,
	)
	p, err := scanPayment(row)
	if err != nil {
		return nil, fmt.Errorf("payment not found")
	}
	return p, nil
}

// UpdatePaymentResult records the outcome of an STK push once Safaricom's
// callback arrives, and atomically flips the linked order to "paid" or
// "payment_failed" to match, in a single transaction.
func (db *DB) UpdatePaymentResult(checkoutRequestID, status, mpesaReceipt, resultDesc string) error {
	return db.withTx(func(tx *sql.Tx) error {
		var orderID int
		err := tx.QueryRow(
			`UPDATE payments SET status = $1, mpesa_receipt = $2, result_desc = $3, updated_at = now()
			 WHERE checkout_request_id = $4
			 RETURNING order_id`,
			status, mpesaReceipt, resultDesc, checkoutRequestID,
		).Scan(&orderID)
		if err != nil {
			return err
		}

		orderStatus := "payment_failed"
		if status == "success" {
			orderStatus = "paid"
		}
		_, err = tx.Exec(`UPDATE orders SET status = $1 WHERE id = $2`, orderStatus, orderID)
		return err
	})
}
