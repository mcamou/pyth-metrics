FROM --platform=linux/amd64 golang:1.22-alpine3.19
WORKDIR /src
COPY . /src
RUN go build -o /bin/pyth-metrics ./main.go

FROM alpine:3.19
COPY --from=0 /bin/pyth-metrics /bin/pyth-metrics
ENTRYPOINT ["/bin/pyth-metrics"]
