# An image with SQLite's command-line shell, sqlite3, on Alpine. make
# sqlite-image builds it. By default it opens /data/app.db; mount a volume at
# /data (-v sqlitedata:/data) so the database outlives the container.
FROM alpine:3.20
RUN apk add --no-cache sqlite
WORKDIR /data
CMD ["sqlite3", "/data/app.db"]
