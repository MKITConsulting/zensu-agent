FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /zensu-agent ./cmd/zensu-agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /zensu-agent /zensu-agent
USER nonroot:nonroot
ENTRYPOINT ["/zensu-agent"]
