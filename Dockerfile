FROM golang:alpine

# Install ca-certificates for HTTPS support
RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN go build -a -o teacup main.go

# Create uploads directory
RUN mkdir -p /app/uploads

# Expose port
EXPOSE 8080

# Run the application
CMD ["./teacup"]

