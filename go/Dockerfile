# Multi-stage build for the automation-agent service.
#
# Only cmd/agent is built into the image — the cmd/playground dev tool is never
# part of a deployed artifact. Static, CGO-free binary on a distroless base.

FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/automation-agent ./cmd/agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/automation-agent /automation-agent
EXPOSE 8080
ENTRYPOINT ["/automation-agent"]
