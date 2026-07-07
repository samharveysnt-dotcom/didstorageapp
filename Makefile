# DIDStorage build/deploy
#
# make build      build the linux/amd64 binary into ./bin/
# make deploy     copy bundle to server, run 02-deploy-app.sh
# make sshpw      build the bootstrap SSH password helper
# make tail-app   tail didapi log on server
# make tail-kam   tail kamailio log on server
# make logs       tail all DIDStorage logs

SERVER ?= root@45.8.93.244
SSH_KEY ?= $(HOME)/.ssh/didstorage_ed25519
SSH := ssh -i $(SSH_KEY) -o IdentitiesOnly=yes -o BatchMode=yes
SCP := scp -i $(SSH_KEY) -o IdentitiesOnly=yes -o BatchMode=yes
BIN := ./bin/didapi-linux-amd64
BUNDLE := /tmp/didstorage-bundle

.PHONY: build deploy push run-deploy sshpw tail-app tail-kam logs migrate clean

build:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o $(BIN) ./cmd/didapi

deploy: build push run-deploy

push:
	$(SSH) $(SERVER) 'rm -rf $(BUNDLE) && mkdir -p $(BUNDLE)/migrations $(BUNDLE)/kamailio $(BUNDLE)/systemd'
	$(SCP) $(BIN) $(SERVER):$(BUNDLE)/didapi-linux-amd64
	$(SCP) -r migrations/. $(SERVER):$(BUNDLE)/migrations/
	$(SCP) kamailio/central.cfg $(SERVER):$(BUNDLE)/kamailio/central.cfg
	$(SCP) deploy/central/systemd/didapi.service $(SERVER):$(BUNDLE)/systemd/
	$(SCP) deploy/central/systemd/rtpengine.service $(SERVER):$(BUNDLE)/systemd/
	$(SCP) deploy/central/02-deploy-app.sh $(SERVER):/root/02-deploy-app.sh

run-deploy:
	$(SSH) $(SERVER) 'chmod +x /root/02-deploy-app.sh && bash /root/02-deploy-app.sh'

sshpw:
	cd scripts/sshpw && go build -o sshpw.exe .

tail-app:
	$(SSH) $(SERVER) 'journalctl -u didapi -f -n 50'

tail-kam:
	$(SSH) $(SERVER) 'journalctl -u kamailio -f -n 50'

logs:
	$(SSH) $(SERVER) 'journalctl -u didapi -u kamailio -u rtpengine -f -n 30'

migrate:
	$(SSH) $(SERVER) 'export PGPASSWORD=$$(cat /root/.pg_didstorage_password) && for f in /opt/didstorage/migrations/*.up.sql; do echo $$f; psql -h 127.0.0.1 -U didstorage -d didstorage -v ON_ERROR_STOP=1 -f $$f; done'

clean:
	rm -rf bin/
