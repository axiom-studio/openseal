FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/skill-slack ./skills/slack

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/skill-slack /skill-slack
EXPOSE 50051
ENTRYPOINT ["/skill-slack"]
