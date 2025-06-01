# Dockerfile for Go backend

# Use an official Go runtime as a parent image
FROM golang:1.22-alpine AS builder

# Set the working directory in the container
WORKDIR /app

# Copy the Go module files and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the application's source code
COPY . .

# Build the Go application
# Assuming your main package is in the root of the backend directory
# and the output executable should be named 'server'
RUN go build -o server .

# Start a new stage from scratch for a smaller image
FROM alpine:latest

WORKDIR /root/

# Copy the Pre-built binary file from the previous stage
COPY --from=builder /app/server .

# Expose port 8080 to the outside world
EXPOSE 8080

# Command to run the executable
CMD ["./server"]
