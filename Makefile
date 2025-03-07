.PHONY: build docker-build docker-push deploy clean

dev:
	go mod tidy &&go run main.go

build:
	CGO_ENABLED=0 GOOS=linux go build -o scheduler .

docker-build: build
	docker build -t Bamboo/cronjob-scheduler:v1 .

docker-push:
	docker push Bamboo/cronjob-scheduler:v1

deploy:
	kubectl apply -f deploy/

clean:
	rm -f scheduler

all: docker-build docker-push deploy

logs:
	kubectl logs -l app=cronjob-scheduler

status:
	kubectl get cronjobs
