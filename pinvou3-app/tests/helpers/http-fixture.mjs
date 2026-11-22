import http from 'node:http';
import { ownServer } from './owned-server.mjs';
export const createServer = (...args) => ownServer(http.createServer(...args));
export default { ...http, createServer };
