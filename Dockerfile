# Stage 1: build frontend
FROM node:22-alpine AS web
WORKDIR /app/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN npm install -g pnpm && pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

# Stage 2: build backend
FROM golang:1.24-alpine AS go
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /app/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -o apiproxy .

# Stage 3: final image
FROM alpine:3.21
RUN apk add --no-cache tzdata ca-certificates
WORKDIR /app
COPY --from=go /app/apiproxy .
EXPOSE 8080
ENTRYPOINT ["./apiproxy"]
