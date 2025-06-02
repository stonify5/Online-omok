# Dockerfile for Go backend

# Use an official Go runtime as a parent image
FROM golang:1.24-alpine AS builder

# Set the working directory in the container
WORKDIR /app

# Copy source code
COPY . .

# Download dependencies
RUN go mod download

# Build the Go application with optimizations for production
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main .

# Start from scratch for minimal image size
FROM scratch

# Set working directory
WORKDIR /app

# Copy SSL certificates for HTTPS requests
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the binary from builder stage
COPY --from=builder /app/main .

# Expose port 8080 to the outside world
EXPOSE 8080

# Command to run the executable
CMD ["/app/main"]
