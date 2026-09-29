# syntax=docker/dockerfile:1

FROM golang:1.25.5 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/stackchan-server ./cmd/stackchan-server

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/stackchan-server /stackchan-server
EXPOSE 8765
USER nonroot:nonroot
# Mount the robot token at /secrets/robot-token and pass -public-url https://<host>.
ENTRYPOINT ["/stackchan-server", "-listen", ":8765", "-token-file", "/secrets/robot-token"]
