FROM golang:1.15.5-alpine3.12

WORKDIR /app
COPY . /app

RUN go build
RUN cp validator-elector /usr/bin/validator-elector

ENTRYPOINT ["validator-elector"]
