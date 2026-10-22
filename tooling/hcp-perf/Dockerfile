FROM golang:1.25.7 AS builder

ENV GOWORK=off CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -mod=readonly -trimpath -buildvcs=false -o /hcp-perf .

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /hcp-perf /hcp-perf
USER 1000:1000
WORKDIR /
ENTRYPOINT ["/hcp-perf"]
