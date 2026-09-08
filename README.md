# 🛒 Jumia MVP — Go Microservices E-Commerce

A minimal but **fully launchable** Jumia-style marketplace built in pure Go.  
**Zero external dependencies. Single binary. Runs anywhere.**

---

## 🚀 Quick Start

```bash
# Clone and run
git clone <repo>
cd jumia-mvp
make run
# → http://localhost:8080
```

Or with Docker:
```bash
make docker
```

---

## 🏗️ Architecture

```
jumia-mvp/
├── cmd/
│   └── main.go              # 🚪 API Gateway — wires all services
├── services/
│   ├── auth/                # 🔐 Auth Service  (register, login, sessions)
│   ├── catalog/             # 📦 Catalog Service (products, search, categories)
│   ├── cart/                # 🛒 Cart Service    (add, update, remove items)
│   └── orders/              # 📋 Orders Service  (checkout, order history)
├── shared/
│   ├── middleware/          # 🛡️  Auth middleware (session extraction)
│   ├── models/              # 📐 Shared data models
│   └── store/               # 🗄️  JSON file store (zero-dep persistence)
└── web/
    └── templates/           # 🎨 HTML templates (responsive UI)
```

### Microservice Design
Each service is a **self-contained Go package** with:
- Its own business logic
- Route registration method `RegisterRoutes(mux, tmpl)`
- No direct dependencies between services (only shared models/store)
- Easy to extract into actual HTTP microservices later

---

## ✅ Features (MVP)

### Buyer
- 🏠 Product catalog with search + category filter  
- 🔍 Product detail pages
- 🛒 Shopping cart (add, update qty, remove)
- 💳 Checkout with delivery address
- 📦 Order history and order details
- 👤 Register / Login / Logout

### Seller
- 🏪 Seller dashboard with inventory stats
- ➕ List new products (name, price, stock, category, emoji icon)
- 🗑️ Delete products

### Platform
- 📱 Fully responsive UI (mobile-first)
- 🔐 Cookie-based session auth (SHA-256 hashed passwords)
- 📊 Stock tracking (auto-decrements on order)
- 💾 JSON file persistence (persists across restarts)
- 🏥 Health check endpoint `/health`

---

## 👤 Demo Accounts

| Role   | Email              | Password |
|--------|--------------------|----------|
| Buyer  | buyer@demo.ke      | demo123  |
| Seller | seller@jumia.ke    | test     |

---

## 🔌 API Endpoints

| Method | Path                        | Description        |
|--------|-----------------------------|--------------------|
| GET    | `/`                         | Homepage / catalog |
| GET    | `/product/:id`              | Product detail     |
| GET    | `/register`                 | Register form      |
| POST   | `/register`                 | Create account     |
| GET    | `/login`                    | Login form         |
| POST   | `/login`                    | Authenticate       |
| GET    | `/logout`                   | Logout             |
| GET    | `/cart`                     | View cart          |
| POST   | `/cart/add`                 | Add to cart        |
| POST   | `/cart/update`              | Update quantity    |
| GET    | `/cart/remove/:id`          | Remove item        |
| GET    | `/checkout`                 | Checkout form      |
| POST   | `/checkout`                 | Place order        |
| GET    | `/orders`                   | Order history      |
| GET    | `/orders/:id`               | Order detail       |
| GET    | `/seller/dashboard`         | Seller dashboard   |
| GET    | `/seller/products/new`      | Add product form   |
| POST   | `/seller/products/new`      | Create product     |
| GET    | `/seller/products/delete/:id` | Delete product   |
| GET    | `/health`                   | Health check       |

---

## ⚙️ Configuration

| Env Var   | Default       | Description        |
|-----------|---------------|--------------------|
| `PORT`    | `8080`        | HTTP port          |
| `DB_PATH` | `./jumia.json`| Data file path     |

---

## 🔮 Scaling Up

This MVP uses JSON file storage — to scale:

1. **Swap store** → Replace `shared/store` with PostgreSQL/MySQL adapter
2. **Split services** → Each service already has its own HTTP handler registration
3. **Add message queue** → Add Kafka/NATS between services for async events
4. **Add Redis** → Session store + cart caching
5. **Container** → Already Dockerized, add docker-compose with Nginx

---

## 📦 Tech Stack

- **Language**: Go 1.22 (stdlib only — zero external deps)
- **Storage**: JSON file (no DB setup required)
- **Templates**: `html/template` (XSS-safe)
- **Auth**: SHA-256 + cookie sessions
- **CSS**: Inline responsive CSS (Jumia orange theme)

---

Built to launch. Scale when ready. 🚀
