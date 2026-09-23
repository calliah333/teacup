FROM golang:alpine

COPY . .

RUN go build -o teacup .

RUN mkdir -p /app/uploads

CMD ["./teacup"]
