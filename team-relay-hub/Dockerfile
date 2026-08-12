FROM node:20-bookworm-slim

ENV NODE_ENV=production
WORKDIR /app/team-relay-hub

COPY team-relay-hub/package.json team-relay-hub/package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts && npm cache clean --force

COPY team-relay-hub/src ./src
COPY docs/team-relay/migrations /app/docs/team-relay/migrations

RUN mkdir -p /var/lib/pinvou-team-relay/attachments \
    && chown -R node:node /app /var/lib/pinvou-team-relay

USER node
EXPOSE 8791
CMD ["npm", "start"]
