import net from 'node:net';
import { ownServer } from './owned-server.mjs';
export const createServer = (...args) => ownServer(net.createServer(...args), 'tcp');
export default { ...net, createServer };
