// Package store is a PostgreSQL-backed data store using plain relational
// columns only — no JSON columns, no marshaling on the hot path.
//
// Compared to the original version, this refactor changes a few method
// signatures where the old signature made an unsafe operation too easy to
// call correctly (see CreateOrder, and the new CreatePendingPayment /
// AttachSTKDetails / MarkPaymentInitFailed helpers that replace directly
// calling CreatePayment from the handler). Every other exported method
// keeps its original signature.
package store

import (
	"agora/shared/models"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
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

// Sentinel errors callers can check with errors.Is.
var (
	// ErrPaymentInProgress means an STK push is already pending for this
	// order — the caller must not start a second one.
	ErrPaymentInProgress = errors.New("a payment is already in progress for this order")
	// ErrProductInactive means the product has been removed by its seller
	// and can no longer be purchased.
	ErrProductInactive = errors.New("product is no longer available")
	// ErrInsufficientStock means the requested quantity exceeds what's on hand.
	ErrInsufficientStock = errors.New("insufficient stock")
)

// Get returns the process-wide singleton DB, connecting on first use.
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
	user := getenvDefault("PGUSER", "agora")
	pass := os.Getenv("PGPASSWORD")
	name := getenvDefault("PGDATABASE", "agorago")
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

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
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
	-- Soft delete: sellers "delete" a product but its id must stay valid
	-- forever because order_items historically reference it. Storefront
	-- queries filter on this; order history never joins to this table.
	is_active   BOOLEAN NOT NULL DEFAULT true,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS products_category_idx ON products (category) WHERE is_active;
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
	id               SERIAL PRIMARY KEY,
	user_id          INTEGER NOT NULL,
	total            NUMERIC(12,2) NOT NULL DEFAULT 0,
	delivery_fee     NUMERIC(12,2) NOT NULL DEFAULT 0,
	status           TEXT NOT NULL DEFAULT 'pending',
	address          TEXT NOT NULL DEFAULT '',
	-- Empty string means "no idempotency key supplied"; the partial unique
	-- index below only enforces uniqueness for non-empty keys, so many
	-- orders can share '' without conflicting.
	idempotency_key  TEXT NOT NULL DEFAULT '',
	created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS orders_user_id_idx ON orders (user_id);
CREATE UNIQUE INDEX IF NOT EXISTS orders_idempotency_key_idx
	ON orders (idempotency_key) WHERE idempotency_key <> '';

CREATE TABLE IF NOT EXISTS order_items (
	id                  SERIAL PRIMARY KEY,
	order_id            INTEGER NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
	product_id          INTEGER NOT NULL,
	quantity            INTEGER NOT NULL,
	price               NUMERIC(12,2) NOT NULL DEFAULT 0,
	-- Snapshot of the product at purchase time. order_items is a historical
	-- record and must render correctly even after the product is deleted
	-- or its price/name/image change — so it never joins to products.
	product_name        TEXT NOT NULL DEFAULT '',
	product_image_url   TEXT NOT NULL DEFAULT '',
	product_category    TEXT NOT NULL DEFAULT ''
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
-- At most one payment may be "pending" per order at a time. This is what
-- makes InitiatePaymentHandler safe against double-click / retry causing
-- two concurrent STK pushes for the same order: see CreatePendingPayment.
CREATE UNIQUE INDEX IF NOT EXISTS payments_one_pending_per_order_idx
	ON payments (order_id) WHERE status = 'pending';
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

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key")
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
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("email already registered")
		}
		return nil, err
	}
	return &u, nil
}

func (db *DB) GetUserByEmail(email string) (*models.User, error) {
	row := db.conn.QueryRow(
		`SELECT `+userCols+` FROM users WHERE LOWER(email) = LOWER($1)`,
		email,
	)
	u, err := scanUser(row)
	if err != nil {
		return nil, fmt.Errorf("user not found")
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

func (db *DB) UpdateUserPasswordHash(userID int, hash string) error {
	_, err := db.conn.Exec(`UPDATE users SET password_hash = $1 WHERE id = $2`, hash, userID)
	return err
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

const productCols = `id, name, description, price, stock, category, image_url, seller_id, is_active, created_at`

func scanProduct(row rowScanner) (models.Product, error) {
	var p models.Product
	err := row.Scan(&p.ID, &p.Name, &p.Description, &p.Price, &p.Stock, &p.Category, &p.ImageURL, &p.SellerID, &p.IsActive, &p.CreatedAt)
	return p, err
}

func (db *DB) CreateProduct(p models.Product) (*models.Product, error) {
	err := db.conn.QueryRow(
		`INSERT INTO products (name, description, price, stock, category, image_url, seller_id, is_active, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, true, now())
		 RETURNING id, is_active, created_at`,
		p.Name, p.Description, p.Price, p.Stock, p.Category, p.ImageURL, p.SellerID,
	).Scan(&p.ID, &p.IsActive, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetProducts returns active, storefront-visible products only.
func (db *DB) GetProducts(category, search string) []models.Product {
	query := `SELECT ` + productCols + ` FROM products WHERE is_active = true`
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

// GetProduct returns an active product by id. Deleted (soft-deleted)
// products behave as "not found" for storefront purposes.
func (db *DB) GetProduct(id int) (*models.Product, error) {
	row := db.conn.QueryRow(`SELECT `+productCols+` FROM products WHERE id = $1 AND is_active = true`, id)
	p, err := scanProduct(row)
	if err != nil {
		return nil, fmt.Errorf("product not found")
	}
	return &p, nil
}

// GetProductsBySellerID returns a seller's active listings, for the
// storefront-facing parts of the seller dashboard.
func (db *DB) GetProductsBySellerID(sellerID int) []models.Product {
	rows, err := db.conn.Query(`SELECT `+productCols+` FROM products WHERE seller_id = $1 AND is_active = true ORDER BY created_at DESC`, sellerID)
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

// DeleteProduct soft-deletes a product (seller-scoped) instead of hard
// deleting it. A hard delete would silently drop the item out of any past
// order's line-item display (order_items had no FK and no snapshot in the
// original schema) and would let stock adjustments target a vanished row.
// Soft delete keeps order history intact and the id stable forever; it
// also removes the product from any shopping cart it's currently sitting
// in, since it can no longer be purchased.
func (db *DB) DeleteProduct(id, sellerID int) {
	db.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE products SET is_active = false WHERE id = $1 AND seller_id = $2`, id, sellerID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Nothing owned by this seller matched — nothing else to do.
			return nil
		}
		_, err = tx.Exec(`DELETE FROM cart_items WHERE product_id = $1`, id)
		return err
	})
}

func (db *DB) GetCategories() []string {
	rows, err := db.conn.Query(`SELECT DISTINCT category FROM products WHERE category <> '' AND is_active = true ORDER BY category`)
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

// UpdateProductStock adjusts a product's stock atomically. Positive delta
// increments, negative decrements; seller-scoped; stock can never go
// negative. Returns the resulting stock level.
func (db *DB) UpdateProductStock(productID, sellerID, delta int) (int, error) {
	if productID <= 0 {
		return 0, fmt.Errorf("invalid product")
	}
	if sellerID <= 0 {
		return 0, fmt.Errorf("invalid seller")
	}

	if delta == 0 {
		var stock int
		err := db.conn.QueryRow(
			`SELECT stock FROM products WHERE id = $1 AND seller_id = $2`,
			productID, sellerID,
		).Scan(&stock)
		if err != nil {
			return 0, fmt.Errorf("product not found")
		}
		return stock, nil
	}

	var stock int

	if delta < 0 {
		err := db.conn.QueryRow(
			`UPDATE products
			 SET stock = stock + $1
			 WHERE id = $2 AND seller_id = $3 AND stock + $1 >= 0
			 RETURNING stock`,
			delta, productID, sellerID,
		).Scan(&stock)

		if err != nil {
			var currentStock int
			checkErr := db.conn.QueryRow(
				`SELECT stock FROM products WHERE id = $1 AND seller_id = $2`,
				productID, sellerID,
			).Scan(&currentStock)
			if checkErr != nil {
				return 0, fmt.Errorf("product not found")
			}
			return 0, fmt.Errorf("stock cannot go below zero")
		}
		return stock, nil
	}

	err := db.conn.QueryRow(
		`UPDATE products SET stock = stock + $1 WHERE id = $2 AND seller_id = $3 RETURNING stock`,
		delta, productID, sellerID,
	).Scan(&stock)
	if err != nil {
		return 0, fmt.Errorf("product not found")
	}
	return stock, nil
}

// restockLocked adds qty back to a product's stock. Must be called with
// tx already holding (or not needing) a lock on the row — used from
// contexts, like a failed-payment callback, where we are unconditionally
// reversing a prior decrement and don't need to re-validate business rules.
func restockLocked(tx *sql.Tx, productID, qty int) error {
	_, err := tx.Exec(`UPDATE products SET stock = stock + $1 WHERE id = $2`, qty, productID)
	return err
}

// ── Cart ──────────────────────────────────────────────

func (db *DB) GetCartItems(userID int) []models.CartItem {
	// INNER JOIN + is_active filter means a product removed by its seller
	// silently drops out of the cart view rather than erroring; DeleteProduct
	// also proactively deletes the cart_items row, so this filter is mostly
	// defense in depth (e.g. products deactivated by some other future path).
	rows, err := db.conn.Query(
		`SELECT ci.id, ci.user_id, ci.product_id, ci.quantity, `+productCols+`
		 FROM cart_items ci JOIN products p ON p.id = ci.product_id AND p.is_active = true
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
			&ci.Product.Stock, &ci.Product.Category, &ci.Product.ImageURL, &ci.Product.SellerID,
			&ci.Product.IsActive, &ci.Product.CreatedAt,
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
		var isActive bool
		if err := tx.QueryRow(`SELECT stock, is_active FROM products WHERE id = $1 FOR UPDATE`, productID).Scan(&stock, &isActive); err != nil {
			return fmt.Errorf("product not found")
		}
		if !isActive {
			return ErrProductInactive
		}
		if stock < qty {
			return ErrInsufficientStock
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
	db.conn.Exec(`UPDATE cart_items SET quantity = $1 WHERE user_id = $2 AND product_id = $3`, qty, userID, productID)
}

func (db *DB) RemoveCartItem(userID, productID int) {
	db.UpdateCartItem(userID, productID, 0)
}

func (db *DB) ClearCart(userID int) {
	db.conn.Exec(`DELETE FROM cart_items WHERE user_id = $1`, userID)
}

// ── Orders ────────────────────────────────────────────

// lockedProduct is what CreateOrder reads (and locks) per product before
// trusting anything about price or availability.
type lockedProduct struct {
	price    float64
	stock    int
	name     string
	imageURL string
	category string
	isActive bool
}

// CreateOrder creates an order from a cart-style item list.
//
// Deliberate changes from the original version:
//
//   - Price is NEVER trusted from the caller. Each product row is read
//     with SELECT ... FOR UPDATE and that price is what gets charged —
//     closing the gap where a stale cart price (or a tampered request)
//     could under/overcharge a customer relative to the seller's current
//     listed price.
//   - o.Total is ignored and recomputed server-side as
//     sum(price*qty) + o.DeliveryFee, for the same reason.
//   - Rows are locked in ascending product_id order (after merging any
//     duplicate product ids in items) regardless of cart insertion order,
//     which prevents a classic deadlock: two concurrent checkouts that
//     share two products but list them in opposite order.
//   - idempotencyKey, if non-empty, makes retrying an identical checkout
//     safe: a second call with the same key returns the original order
//     instead of creating a duplicate and double-decrementing stock.
//   - order_items snapshots product name/image/category at purchase time
//     instead of relying on a live join to products, so order history
//     survives the product later being edited or deleted.
func (db *DB) CreateOrder(o models.Order, items []models.OrderItem, idempotencyKey string) (*models.Order, error) {
	if len(items) == 0 {
		return nil, fmt.Errorf("order contains no items")
	}
	if strings.TrimSpace(o.Address) == "" {
		return nil, fmt.Errorf("delivery address is required")
	}
	if o.DeliveryFee < 0 {
		return nil, fmt.Errorf("invalid delivery fee")
	}

	// Merge duplicate product ids (e.g. the same product appearing twice in
	// a cart snapshot) so each row is locked exactly once, then sort by
	// product id for a consistent, deadlock-safe lock order.
	qtyByProduct := make(map[int]int, len(items))
	for _, it := range items {
		if it.ProductID <= 0 {
			return nil, fmt.Errorf("invalid product")
		}
		if it.Quantity <= 0 {
			return nil, fmt.Errorf("invalid quantity for product %d", it.ProductID)
		}
		qtyByProduct[it.ProductID] += it.Quantity
	}
	productIDs := make([]int, 0, len(qtyByProduct))
	for id := range qtyByProduct {
		productIDs = append(productIDs, id)
	}
	sort.Ints(productIDs)

	var result models.Order

	err := db.withTx(func(tx *sql.Tx) error {
		if idempotencyKey != "" {
			var existingID int
			err := tx.QueryRow(
				`SELECT id FROM orders WHERE idempotency_key = $1 AND user_id = $2`,
				idempotencyKey, o.UserID,
			).Scan(&existingID)
			if err == nil {
				// Already created by an earlier, possibly-retried call —
				// return that order unchanged rather than creating another.
				existing, loadErr := db.getOrderTx(tx, existingID, o.UserID)
				if loadErr != nil {
					return loadErr
				}
				result = *existing
				return nil
			}
			if err != sql.ErrNoRows {
				return err
			}
		}

		locked := make(map[int]lockedProduct, len(productIDs))
		var total float64

		for _, pid := range productIDs {
			qty := qtyByProduct[pid]

			var lp lockedProduct
			err := tx.QueryRow(
				`SELECT price, stock, name, image_url, category, is_active
				 FROM products WHERE id = $1 FOR UPDATE`,
				pid,
			).Scan(&lp.price, &lp.stock, &lp.name, &lp.imageURL, &lp.category, &lp.isActive)
			if err != nil {
				return fmt.Errorf("product %d not found", pid)
			}
			if !lp.isActive {
				return fmt.Errorf("product %d (%s): %w", pid, lp.name, ErrProductInactive)
			}
			if lp.stock < qty {
				return fmt.Errorf("product %d (%s): %w", pid, lp.name, ErrInsufficientStock)
			}

			locked[pid] = lp
			total += lp.price * float64(qty)
		}

		total += o.DeliveryFee
		if total <= 0 {
			return fmt.Errorf("order total must be positive")
		}

		var orderID int
		var createdAt time.Time
		if err := tx.QueryRow(
			`INSERT INTO orders (user_id, total, delivery_fee, status, address, idempotency_key, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, now())
			 RETURNING id, created_at`,
			o.UserID, total, o.DeliveryFee, "pending", o.Address, idempotencyKey,
		).Scan(&orderID, &createdAt); err != nil {
			return err
		}

		resultItems := make([]models.OrderItem, 0, len(productIDs))
		for _, pid := range productIDs {
			qty := qtyByProduct[pid]
			lp := locked[pid]

			var itemID int
			if err := tx.QueryRow(
				`INSERT INTO order_items (order_id, product_id, quantity, price, product_name, product_image_url, product_category)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)
				 RETURNING id`,
				orderID, pid, qty, lp.price, lp.name, lp.imageURL, lp.category,
			).Scan(&itemID); err != nil {
				return err
			}

			if _, err := tx.Exec(`UPDATE products SET stock = stock - $1 WHERE id = $2`, qty, pid); err != nil {
				return err
			}

			resultItems = append(resultItems, models.OrderItem{
				ID: itemID, OrderID: orderID, ProductID: pid, Quantity: qty, Price: lp.price,
				Product: models.Product{ID: pid, Name: lp.name, ImageURL: lp.imageURL, Category: lp.category, Price: lp.price},
			})
		}

		result = models.Order{
			ID: orderID, UserID: o.UserID, Total: total, DeliveryFee: o.DeliveryFee,
			Status: "pending", Address: o.Address, Items: resultItems, CreatedAt: createdAt,
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return &result, nil
}

// scanOrderItemSnapshot reads one order_items row into a models.OrderItem,
// populating Product entirely from the purchase-time snapshot columns —
// there is deliberately no join to products here (see CreateOrder's doc
// comment on why order_items is self-contained).
func scanOrderItemSnapshot(row rowScanner) (models.OrderItem, error) {
	var oi models.OrderItem
	var name, imageURL, category string
	err := row.Scan(&oi.ID, &oi.OrderID, &oi.ProductID, &oi.Quantity, &oi.Price, &name, &imageURL, &category)
	if err != nil {
		return oi, err
	}
	oi.Product = models.Product{ID: oi.ProductID, Name: name, ImageURL: imageURL, Category: category, Price: oi.Price}
	return oi, nil
}

const orderItemCols = `id, order_id, product_id, quantity, price, product_name, product_image_url, product_category`

func (db *DB) getOrderItemsTx(tx *sql.Tx, orderID int) ([]models.OrderItem, error) {
	rows, err := tx.Query(`SELECT `+orderItemCols+` FROM order_items WHERE order_id = $1 ORDER BY id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []models.OrderItem
	for rows.Next() {
		oi, err := scanOrderItemSnapshot(rows)
		if err != nil {
			continue
		}
		items = append(items, oi)
	}
	return items, nil
}

func (db *DB) getOrderTx(tx *sql.Tx, orderID, userID int) (*models.Order, error) {
	var o models.Order
	err := tx.QueryRow(
		`SELECT id, user_id, total, delivery_fee, status, address, created_at
		 FROM orders WHERE id = $1 AND user_id = $2`,
		orderID, userID,
	).Scan(&o.ID, &o.UserID, &o.Total, &o.DeliveryFee, &o.Status, &o.Address, &o.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("order not found")
	}
	items, err := db.getOrderItemsTx(tx, orderID)
	if err != nil {
		return nil, err
	}
	o.Items = items
	return &o, nil
}

// GetOrders lists a user's orders, most recent first. This does a single
// joined query rather than one order_items query per order (the original
// version's attachOrderItems-in-a-loop was an N+1 query pattern).
func (db *DB) GetOrders(userID int) []models.Order {
	rows, err := db.conn.Query(
		`SELECT o.id, o.user_id, o.total, o.delivery_fee, o.status, o.address, o.created_at,
		        oi.id, oi.order_id, oi.product_id, oi.quantity, oi.price,
		        oi.product_name, oi.product_image_url, oi.product_category
		 FROM orders o
		 LEFT JOIN order_items oi ON oi.order_id = o.id
		 WHERE o.user_id = $1
		 ORDER BY o.created_at DESC, oi.id ASC`,
		userID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []models.Order
	index := make(map[int]int) // order id -> index in result

	for rows.Next() {
		var o models.Order
		var itemID, orderID, productID, quantity sql.NullInt64
		var price sql.NullFloat64
		var name, imageURL, category sql.NullString

		if err := rows.Scan(
			&o.ID, &o.UserID, &o.Total, &o.DeliveryFee, &o.Status, &o.Address, &o.CreatedAt,
			&itemID, &orderID, &productID, &quantity, &price, &name, &imageURL, &category,
		); err != nil {
			continue
		}

		i, ok := index[o.ID]
		if !ok {
			result = append(result, o)
			i = len(result) - 1
			index[o.ID] = i
		}

		if itemID.Valid {
			result[i].Items = append(result[i].Items, models.OrderItem{
				ID: int(itemID.Int64), OrderID: int(orderID.Int64), ProductID: int(productID.Int64),
				Quantity: int(quantity.Int64), Price: price.Float64,
				Product: models.Product{ID: int(productID.Int64), Name: name.String, ImageURL: imageURL.String, Category: category.String, Price: price.Float64},
			})
		}
	}
	return result
}

func (db *DB) GetOrder(orderID, userID int) (*models.Order, error) {
	var o models.Order
	err := db.conn.QueryRow(
		`SELECT id, user_id, total, delivery_fee, status, address, created_at
		 FROM orders WHERE id = $1 AND user_id = $2`,
		orderID, userID,
	).Scan(&o.ID, &o.UserID, &o.Total, &o.DeliveryFee, &o.Status, &o.Address, &o.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("order not found")
	}

	rows, err := db.conn.Query(`SELECT `+orderItemCols+` FROM order_items WHERE order_id = $1 ORDER BY id`, orderID)
	if err != nil {
		return &o, nil
	}
	defer rows.Close()
	for rows.Next() {
		oi, err := scanOrderItemSnapshot(rows)
		if err != nil {
			continue
		}
		o.Items = append(o.Items, oi)
	}
	return &o, nil
}

// UpdateOrderStatus sets an order's status directly (e.g. "shipped").
// It does NOT restock — it's a low-level primitive for status transitions
// that have nothing to do with payment/inventory. To cancel a pending
// order and release its reserved stock, use CancelOrder instead.
func (db *DB) UpdateOrderStatus(orderID int, status string) error {
	_, err := db.conn.Exec(`UPDATE orders SET status = $1 WHERE id = $2`, status, orderID)
	return err
}

// CancelOrder cancels a still-pending order (seller/customer-initiated,
// e.g. before payment) and restocks every item on it. Only orders in
// "pending" status can be cancelled this way — a paid order needs a
// refund flow, not a plain status flip, so this intentionally does not
// touch "paid" orders.
func (db *DB) CancelOrder(orderID, userID int) error {
	return db.withTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(
			`UPDATE orders SET status = 'cancelled' WHERE id = $1 AND user_id = $2 AND status = 'pending'`,
			orderID, userID,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("order not found or not cancellable")
		}
		items, err := db.getOrderItemsTx(tx, orderID)
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := restockLocked(tx, it.ProductID, it.Quantity); err != nil {
				return err
			}
		}
		return nil
	})
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

// CreatePendingPayment atomically reserves the right to attempt a payment
// for this order: it inserts a 'pending' payment row, relying on the
// payments_one_pending_per_order_idx partial unique index to make the
// insert fail (ErrPaymentInProgress) if another pending payment already
// exists. Call this BEFORE calling Safaricom's STK push API, and only
// call STKPush if this succeeds — that way at most one push is ever in
// flight for a given order, without holding a DB transaction open across
// the network call to Safaricom.
//
// ROUNDING NOTE: amount must be the exact whole-shilling integer you are
// about to request from STKPush, not the order's unrounded float total —
// the M-Pesa callback later compares this value against what Safaricom
// reports as paid, so a mismatch here makes every successful payment look
// like a fraud/mismatch failure.
func (db *DB) CreatePendingPayment(orderID int, phone string, amount int) (*models.Payment, error) {
	var p models.Payment
	err := db.conn.QueryRow(
		`INSERT INTO payments (order_id, phone, amount, status, created_at, updated_at)
		 VALUES ($1, $2, $3, 'pending', now(), now())
		 ON CONFLICT (order_id) WHERE status = 'pending' DO NOTHING
		 RETURNING `+paymentCols,
		orderID, phone, amount,
	).Scan(&p.ID, &p.OrderID, &p.Phone, &p.Amount, &p.MerchantRequestID, &p.CheckoutRequestID,
		&p.MpesaReceipt, &p.Status, &p.ResultDesc, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrPaymentInProgress
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// AttachSTKDetails records Safaricom's merchant/checkout request ids on an
// already-created pending payment, once STKPush has returned successfully.
func (db *DB) AttachSTKDetails(paymentID int, merchantRequestID, checkoutRequestID string) error {
	_, err := db.conn.Exec(
		`UPDATE payments SET merchant_request_id = $1, checkout_request_id = $2, updated_at = now() WHERE id = $3`,
		merchantRequestID, checkoutRequestID, paymentID,
	)
	return err
}

// MarkPaymentInitFailed is called when CreatePendingPayment succeeded but
// the subsequent STKPush call itself failed (network error, Safaricom
// rejected the request, etc). It moves the row out of 'pending' — using a
// status distinct from the callback's 'failed' — so the unique pending-
// payment index no longer blocks the customer from retrying.
func (db *DB) MarkPaymentInitFailed(paymentID int, reason string) error {
	_, err := db.conn.Exec(
		`UPDATE payments SET status = 'init_failed', result_desc = $1, updated_at = now() WHERE id = $2`,
		reason, paymentID,
	)
	return err
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
// callback arrives. In one transaction it:
//  1. flips the payment row from 'pending' to 'success'/'failed' — guarded
//     by "AND status = 'pending'" so a duplicate/replayed callback for an
//     already-resolved payment is a safe no-op instead of double-processing;
//  2. flips the linked order to 'paid' or 'payment_failed', guarded the
//     same way;
//  3. on failure, restocks every item on the order — this is the fix for
//     the original version, which decremented stock at order-creation time
//     but never gave it back when the payment that was supposed to
//     complete the sale didn't go through.
func (db *DB) UpdatePaymentResult(checkoutRequestID, status, mpesaReceipt, resultDesc string) error {
	return db.withTx(func(tx *sql.Tx) error {
		var orderID int
		err := tx.QueryRow(
			`UPDATE payments
			 SET status = $1, mpesa_receipt = $2, result_desc = $3, updated_at = now()
			 WHERE checkout_request_id = $4 AND status = 'pending'
			 RETURNING order_id`,
			status, mpesaReceipt, resultDesc, checkoutRequestID,
		).Scan(&orderID)
		if err == sql.ErrNoRows {
			// Either unknown checkout_request_id, or this payment was
			// already resolved by an earlier callback — nothing to do.
			return nil
		}
		if err != nil {
			return err
		}

		orderStatus := "payment_failed"
		if status == "success" {
			orderStatus = "paid"
		}

		res, err := tx.Exec(
			`UPDATE orders SET status = $1 WHERE id = $2 AND status = 'pending'`,
			orderStatus, orderID,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Order was already moved out of 'pending' by something else
			// (e.g. cancelled) between payment creation and this callback.
			// Don't restock here — whatever moved it out is responsible
			// for its own inventory handling.
			return nil
		}

		if orderStatus == "payment_failed" {
			items, err := db.getOrderItemsTx(tx, orderID)
			if err != nil {
				return err
			}
			for _, it := range items {
				if err := restockLocked(tx, it.ProductID, it.Quantity); err != nil {
					return err
				}
			}
		}
		return nil
	})
}