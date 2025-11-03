FROM golang:alpine

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

