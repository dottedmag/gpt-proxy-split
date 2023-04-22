NAME=gpt-proxy-split
LOCEXE=${NAME}.linux-amd64
SVC=${NAME}.service

all: ${LOCEXE}

run:
	. ./env && export OPENAPI_KEY && go run .

${LOCEXE}: db.go main.go proxy.go
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $@ .

deploy: ${LOCEXE}
	ssh tea mkdir -p ${NAME} .config/systemd/user
	rsync ${LOCEXE} tea:${NAME}/${NAME}
	rsync ${SVC} tea:.config/systemd/user/${SVC}
	ssh tea chmod +x ${NAME}/${NAME}
	ssh tea systemctl --user daemon-reload
	ssh tea systemctl --user restart ${NAME}

clean:
	-rm ${LOCEXE}
