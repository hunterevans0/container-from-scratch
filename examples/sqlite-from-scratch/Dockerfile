# SQLite's shell, sqlite3, in an image FROM scratch: one static binary and
# nothing else. make sqlite-from-scratch-image builds it. Compare it with
# examples/sqlite (Alpine plus apk's sqlite) in runt images.
#
# Stage 1 compiles sqlite3 from SQLite's amalgamation, its whole source in two
# C files. Alpine's C library, musl, links statically without trouble, so the
# binary needs no shared libraries and runs in an empty image.
FROM alpine:3.20 AS build
RUN apk add --no-cache build-base

# sqlite.org publishes SHA3-256 digests, but ADD checks SHA-256, so this one
# was worked out from the zip after checking it against SQLite's SHA3-256
# (92281c86...28e30). To move to another version, change both lines.
ADD --checksum=sha256:0a9c8e34352dad46006aa82836d5383bff9dc6bc9ce5a60553c05aa30e6ad1f9 \
    https://www.sqlite.org/2026/sqlite-amalgamation-3540100.zip /src/sqlite.zip

# -static: no shared libraries. -Os: optimize for size. -s: strip symbols.
# SQLITE_THREADSAFE=0: the shell is single-threaded, so no pthreads.
# SQLITE_OMIT_LOAD_EXTENSION: no dlopen, which a static binary can't use.
# Without readline, the shell has no history or arrow keys; line editing is
# only what the terminal does (backspace).
WORKDIR /src
RUN unzip -q sqlite.zip && \
    cd sqlite-amalgamation-3540100 && \
    gcc -static -Os -s \
        -DSQLITE_THREADSAFE=0 -DSQLITE_OMIT_LOAD_EXTENSION \
        -o /sqlite3 shell.c sqlite3.c -lm

# Stage 2: only the binary. No shell, no package manager, no libc, no
# /etc/passwd. Whatever an attacker gets into, there is nothing else to run.
FROM scratch
COPY --from=build /sqlite3 /sqlite3
WORKDIR /data
CMD ["/sqlite3", "/data/app.db"]
