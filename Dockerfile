# Fire Air Hockey — static build, distroless-style runtime.
# Frontend is embedded in the binary via go:embed, so the runtime image
# carries just the one binary and runs it as a non-root user.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/fire-airhockey .

FROM alpine:3.21
RUN adduser -D -H -u 10001 app
USER app
EXPOSE 8080
ENV PORT=8080
COPY --from=build /out/fire-airhockey /fire-airhockey
ENTRYPOINT ["/fire-airhockey"]
