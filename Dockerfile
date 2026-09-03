# zae in a container: nothing to install, handy in CI.
# distroless/static ships CA certificates, which doctor's TLS checks need.
FROM golang:1.24 AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /zae .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /zae /zae
ENTRYPOINT ["/zae"]
