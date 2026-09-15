package models

import "time"

type User struct {
	ID           int       `json:"id"`
	Name         string    `json:"name"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	CreatedAt    time.Time `json:"created_at"`
}

type Product struct {
	ID          int       `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Price       float64   `json:"price"`
	Stock       int       `json:"stock"`
	Category    string    `json:"category"`
	ImageURL    string    `json:"image_url"`
	SellerID    int       `json:"seller_id"`
	// IsActive is false for soft-deleted products. Kept around (rather than
	// hard-deleted) so historical order_items / order display never breaks —
	// see store.DeleteProduct.
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

type CartItem struct {
	ID        int     `json:"id"`
	UserID    int     `json:"user_id"`
	ProductID int     `json:"product_id"`
	Quantity  int     `json:"quantity"`
	Product   Product `json:"product"`
}

type Order struct {
	ID     int     `json:"id"`
	UserID int     `json:"user_id"`
	Total  float64 `json:"total"`
	Status string  `json:"status"`
	// DeliveryFee is added to the sum of line items by store.CreateOrder to
	// produce Total. Callers should NOT try to bake delivery fee into a
	// pre-computed Total — CreateOrder recomputes Total itself from
	// authoritative product prices + this fee, and ignores any Total the
	// caller supplies, so client-side tampering with prices can't succeed.
	DeliveryFee float64 `json:"delivery_fee"`
	Address     string  `json:"address"`
	// IdempotencyKey, when non-empty, lets CreateOrder be called safely more
	// than once for the same logical checkout attempt (double submit, retry
	// after a timed-out response) without creating duplicate orders or
	// double-decrementing stock. Generate one per checkout page-load
	// (e.g. a UUID in a hidden form field) and pass the same value on retry.
	IdempotencyKey string      `json:"-"`
	Items          []OrderItem `json:"items"`
	CreatedAt      time.Time   `json:"created_at"`
}

// OrderItem is an immutable historical record of what was purchased.
// Price/ProductName/ProductImageURL/ProductCategory are captured at the
// moment of purchase (see store.CreateOrder) and never change afterwards,
// even if the underlying product's price changes or the product is later
// deleted. Product is populated from that snapshot when read back — it is
// NOT a live join to the products table.
type OrderItem struct {
	ID        int     `json:"id"`
	OrderID   int     `json:"order_id"`
	ProductID int     `json:"product_id"`
	Quantity  int     `json:"quantity"`
	Price     float64 `json:"price"`
	Product   Product `json:"product"`
}

type Session struct {
	ID        string    `json:"id"`
	UserID    int       `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Payment represents one M-Pesa STK Push attempt tied to an order.
// Status is one of: "pending", "success", "failed".
//
// Amount is the exact whole-shilling amount that was actually requested
// from Safaricom (i.e. the rounded value passed to STKPush), NOT the
// order's unrounded float total — see store.CreatePendingPayment. The
// callback handler compares this field against the amount Safaricom
// reports as paid, so it must match what was actually requested or every
// successful payment will be misclassified as a mismatch.
type Payment struct {
	ID                int       `json:"id"`
	OrderID           int       `json:"order_id"`
	Phone             string    `json:"phone"`
	Amount            float64   `json:"amount"`
	MerchantRequestID string    `json:"merchant_request_id"`
	CheckoutRequestID string    `json:"checkout_request_id"`
	MpesaReceipt      string    `json:"mpesa_receipt"`
	Status            string    `json:"status"`
	ResultDesc        string    `json:"result_desc"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}