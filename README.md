# crawler-api

A REST API built with [Gin](https://github.com/gin-gonic/gin) (Go) that serves data for the Crawler UI. It persists data collected by bots into a MongoDB database and exposes endpoints for:

- **Dashboard** – aggregate statistics across all projects
- **Projects** – CRUD management of monitored websites
- **SEO Analytics** – SEO metadata collected by the crawler bot (title, meta description, H-tags, canonical URL, Open Graph, status codes, load times, …)
- **Prerender Analytics** – JavaScript prerender cache entries submitted by the bot (rendered HTML, cache hit/miss stats, render time, …)
- **Settings** – key/value application configuration

---

## Quick start with Docker Compose

```bash
# 1. Copy and adjust the environment file (optional)
cp .env.example .env   # see Environment variables below

# 2. Build and start the API + MongoDB
docker compose up --build -d

# 3. The API is now available at http://localhost:9090
curl http://localhost:9090/health
```

### Environment variables

| Variable              | Default                          | Description                        |
|-----------------------|----------------------------------|------------------------------------|
| `MONGO_ROOT_USER`     | `admin`                          | MongoDB root username              |
| `MONGO_ROOT_PASSWORD` | `changeme`                       | MongoDB root password (**change!**)|
| `MONGO_DB`            | `crawler`                        | Database name                      |
| `SERVER_PORT`         | `9090`                           | Port the API listens on            |
| `GIN_MODE`            | `release`                        | `debug` or `release`               |
| `CORS_ORIGINS`        | `*`                              | Allowed CORS origins (comma-separated). Use `*` for dev, restrict for production. |

---

## API Endpoints

All endpoints are prefixed with `/api/v1`.

| Method   | Path                    | Description                             |
|----------|-------------------------|-----------------------------------------|
| `GET`    | `/health`               | Health check                            |
| `GET`    | `/api/v1/dashboard`     | Aggregated dashboard statistics         |
| `GET`    | `/api/v1/projects`      | List all projects                       |
| `POST`   | `/api/v1/projects`      | Create a project                        |
| `GET`    | `/api/v1/projects/:id`  | Get a project                           |
| `PUT`    | `/api/v1/projects/:id`  | Update a project                        |
| `DELETE` | `/api/v1/projects/:id`  | Delete a project                        |
| `GET`    | `/api/v1/seo`           | List SEO records (filter: `project_id`) |
| `POST`   | `/api/v1/seo`           | Ingest SEO data from the bot            |
| `GET`    | `/api/v1/seo/:id`       | Get a SEO record                        |
| `DELETE` | `/api/v1/seo/:id`       | Delete a SEO record                     |
| `GET`    | `/api/v1/prerender`     | List prerender records (filter: `project_id`) |
| `POST`   | `/api/v1/prerender`     | Ingest prerender data from the bot      |
| `GET`    | `/api/v1/prerender/:id` | Get a prerender record                  |
| `DELETE` | `/api/v1/prerender/:id` | Delete a prerender record               |
| `GET`    | `/api/v1/settings`      | List all settings                       |
| `PUT`    | `/api/v1/settings`      | Create or update a setting (upsert)     |
| `DELETE` | `/api/v1/settings/:key` | Delete a setting                        |

---

## Running locally (without Docker)

```bash
# Prerequisites: Go 1.25+ and a running MongoDB instance

export MONGO_URI="mongodb://localhost:27017"
export MONGO_DB="crawler"
export GIN_MODE="debug"

go run .
```

## Project structure

```
.
├── config/         # Environment-based configuration
├── db/             # MongoDB connection helpers
├── handlers/       # Gin route handler implementations
├── middleware/     # CORS and structured-logging middleware
├── models/         # BSON/JSON data models
├── routes/         # Route registration
├── main.go         # Entry point with graceful shutdown
├── Dockerfile
└── docker-compose.yml
```
