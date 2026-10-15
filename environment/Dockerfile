FROM public.ecr.aws/x8v8d7g8/mars-base:latest
WORKDIR /app
ARG REPO_URL=https://github.com/mahendrapaipuri/grafana-dashboard-reporter-app
ARG BASE_SHA=4a1185b9c9fd2a0b1abd96786f2335fbc46a302c
RUN git clone "$REPO_URL" . \
 && DEFAULT="$(git remote show origin | sed -n 's/.*HEAD branch: //p')" \
 && git checkout -B "$DEFAULT" "$BASE_SHA" \
 && git remote remove origin \
 && git reflog expire --expire=now --all \
 && git gc --prune=now
RUN go mod download
CMD ["bash"]
