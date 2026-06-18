FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /wally-preview .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /wally-preview /wally-preview
EXPOSE 8088
USER nonroot:nonroot
ENTRYPOINT ["/wally-preview"]
