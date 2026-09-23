FROM node:20-bookworm-slim

ENV NODE_ENV=production
WORKDIR /app/update-release-console

COPY package.json package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts && npm cache clean --force

COPY src ./src
COPY public ./public

RUN mkdir -p /var/lib/pinvou-update-console /srv/pinvou3 \
    && chown -R node:node /app /var/lib/pinvou-update-console /srv/pinvou3

USER node
EXPOSE 8792
CMD ["npm", "start"]
