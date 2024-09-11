build-linux:
	GOOS=linux GOARCH=amd64 go build -o ./bin/media-linux-amd64 .

build-mac:
	GOOS=darwin GOARCH=arm64 go build -o ./bin/media-mac-arm64 .

build-windows:
	GOOS=windows GOARCH=arm64 go build -o ./bin/media-windows-arm64.exe .

build-all: build-linux build-mac build-windows

push-linux:
	rsync -avzP ./index.html ubuntu@13.235.90.84:/home/ubuntu
