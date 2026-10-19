FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /feed ./cmd/feed

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /feed /feed
USER nonroot:nonroot
ENTRYPOINT ["/feed"]
