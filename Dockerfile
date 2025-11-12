FROM golang:alpine

COPY . .

RUN go build -a -o teacup main.go

RUN mkdir -p /app/uploads

CMD ["./teacup"]

