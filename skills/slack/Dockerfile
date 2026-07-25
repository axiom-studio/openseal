FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache build-base
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w -linkmode external -extldflags -static" \
    -o /out/skill-slack-conversations ./skills/slack

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/skill-slack-conversations /skill-slack-conversations
EXPOSE 50051
ENTRYPOINT ["/skill-slack-conversations"]
