# Build from the repo root:  gcloud builds submit --config cmd/ob-mcp/cloudbuild.yaml ...
# The upload is limited by .gcloudignore to code only; no trading data.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY config ./config
COPY books ./books
COPY internal ./internal
COPY cmd/ob-mcp ./cmd/ob-mcp
COPY cmd/ob ./cmd/ob
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.commit=${COMMIT}" -o /ob-mcp ./cmd/ob-mcp
# /ob too: the options-book-cred-refresh job runs `ob cred refresh robinhood` from this image.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ob ./cmd/ob

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ob-mcp /ob-mcp
COPY --from=build /ob /ob
USER nonroot:nonroot
ENTRYPOINT ["/ob-mcp"]
