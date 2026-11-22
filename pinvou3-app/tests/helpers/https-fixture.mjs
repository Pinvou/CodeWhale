import https from 'node:https';
import { ownServer } from './owned-server.mjs';
export const createServer = (...args) => ownServer(https.createServer(...args));
export default { ...https, createServer };
