# Use an official lightweight Go image
FROM golang:1.21-alpine

# Set working directory inside the container
WORKDIR /app

# Copy the Go module files and the source code
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Build the application
RUN go build -o main .

# Command to run the application
CMD ["./main"]
