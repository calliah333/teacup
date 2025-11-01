# Build stage
FROM golang:alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN go build -a -installsuffix cgo -o teacup main.go

# Runtime stage
FROM alpine:latest

# Install sqlite and ca-certificates for HTTPS support
RUN apk --no-cache add ca-certificates sqlite

WORKDIR /app

# Create uploads directory
RUN mkdir -p /app/uploads

# Copy binary from builder
COPY --from=builder /app/teacup /app/teacup
COPY --from=builder /app/index.html /app/index.html
COPY --from=builder /app/config.txt /app/config.txt

# Expose port
EXPOSE 8080

# Run the application
CMD ["./teacup"]

