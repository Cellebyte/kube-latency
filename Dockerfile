FROM golang:alpine as build
ARG GIT_STATE ""
ARG GIT_COMMIT ""
ARG APP_VERSION ""
WORKDIR /app
ADD . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -tags netgo -o kube-latency -ldflags "-X main.AppGitState=${GIT_STATE} -X main.AppGitCommit=${GIT_COMMIT} -X main.AppVersion=${APP_VERSION}"

FROM scratch as final
COPY --from=build /app/kube-latency .
CMD ["/kube-latency"]
LABEL org.label-schema.vcs-ref=$GIT_COMMIT \
      org.label-schema.vcs-url="https://github.com/simonswine/kube-latency" \
      org.label-schema.license="Apache-2.0"
