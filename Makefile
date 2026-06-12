orgmail: main.go
	go build -o orgmail main.go

serve:
	./orgmail mail.org 127.0.0.1:1143

