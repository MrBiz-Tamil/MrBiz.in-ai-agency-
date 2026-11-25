// Real Postgres (PGlite) over the wire protocol. Single-connection: use pool_max_conns=1.
//   node server.mjs [port]            (default 25432)
//   PGTEST_DATA=./.data node server.mjs   persist to a directory (default: in memory)
// Connect with:
//   postgres://postgres@127.0.0.1:25432/postgres?sslmode=disable&pool_max_conns=1&default_query_exec_mode=describe_exec
// Do NOT register this port with a tool that probes it over HTTP: garbage input aborts PGlite.
import { PGlite } from '@electric-sql/pglite';
import { PGLiteSocketServer } from '@electric-sql/pglite-socket';

const port = Number(process.argv[2] || process.env.PGTEST_PORT || 25432);
const db = await PGlite.create(process.env.PGTEST_DATA || undefined);
const server = new PGLiteSocketServer({ db, port, host: '127.0.0.1', connectionQueueTimeout: 60000 });
await server.start();
console.log(`pgtest ready on 127.0.0.1:${port}${process.env.PGTEST_DATA ? ` (data: ${process.env.PGTEST_DATA})` : ' (in memory)'}`);

// A client that disconnects abruptly (ECONNRESET) must not take the database down.
process.on('uncaughtException', (err) => {
	if (err && (err.code === 'ECONNRESET' || err.code === 'EPIPE')) {
		console.warn(`ignored client error: ${err.code}`);
		return;
	}
	console.error(err);
	process.exit(1);
});

const stop = async () => {
	await server.stop();
	await db.close();
	process.exit(0);
};
process.on('SIGINT', stop);
process.on('SIGTERM', stop);
