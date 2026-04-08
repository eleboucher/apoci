FROM public.ecr.aws/docker/library/alpine:3.21
RUN apk add --no-cache curl coreutils
COPY federation.sh /tests/federation.sh
ENTRYPOINT ["sh", "/tests/federation.sh"]
