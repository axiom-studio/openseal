import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import { spawn, type ChildProcess } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdirSync } from "node:fs";
import { resolve } from "node:path";
import { createInterface } from "node:readline";
import type { IncomingMessage } from "node:http";

async function requestBody(req: IncomingMessage) {
  const chunks: Buffer[] = [];
  let length = 0;
  for await (const chunk of req) {
    length += chunk.length;
    if (length > 131072) throw new Error("Request too large");
    chunks.push(chunk);
  }
  return Buffer.concat(chunks);
}
function providerSettings(directory: string, input: unknown): Promise<unknown> {
  return new Promise((resolveResult, reject) => {
    const binary = resolve(
      `crates/daemon-host/target/debug/openseal-provider-settings${process.platform === "win32" ? ".exe" : ""}`,
    );
    const bridge = spawn(binary, [directory], {
      stdio: ["pipe", "pipe", "ignore"],
    });
    let output = "";
    bridge.stdout.on("data", (chunk) => {
      output += chunk.toString();
    });
    bridge.once("error", () =>
      reject(
        new Error(
          "Provider settings bridge is unavailable. Restart the development server.",
        ),
      ),
    );
    bridge.once("close", () => {
      try {
        const response = JSON.parse(output);
        response.error
          ? reject(new Error(response.error))
          : resolveResult(response.settings);
      } catch {
        reject(new Error("Provider settings could not be read."));
      }
    });
    bridge.stdin.on("error", () => {});
    bridge.stdin.end(JSON.stringify(input));
  });
}

// Browser development has an isolated workspace; credentials stay in the server process.
function localDaemon(): Plugin {
  return {
    name: "openseal-local-daemon",
    async configureServer(server) {
      const directory = resolve(
        process.env.OPENSEAL_DEV_WORKSPACE || ".dev-workspace",
      );
      mkdirSync(directory, { recursive: true });
      let child: ChildProcess | undefined;
      let endpoint = "";
      let error = "";
      let token = "";
      let saving = false;
      let closing = false;
      async function start() {
        endpoint = "";
        error = "";
        token = randomBytes(32).toString("hex");
        const process = spawn(
          resolve(globalThis.process.env.OPENSEAL_DAEMON_PATH || "../openseal"),
          [
            "daemon",
            "--config",
            resolve(directory, "daemon.yaml"),
            "--context",
            resolve(directory, "context.yaml"),
            "--listen",
            "127.0.0.1:0",
            "--standalone-operator",
            "--desktop-operator",
          ],
          {
            cwd: directory,
            env: { ...globalThis.process.env, OPENSEAL_API_TOKEN: token },
            stdio: ["ignore", "pipe", "ignore"],
          },
        );
        child = process;
        return new Promise<void>((ready, reject) => {
          const fail = (message: string) => {
            if (child !== process) return;
            error = message;
            endpoint = "";
            clearTimeout(timeout);
            reject(new Error(message));
          };
          const timeout = setTimeout(
            () =>
              fail(
                "OpenSeal did not start. Check the workspace configuration.",
              ),
            30_000,
          );
          process.once("error", () =>
            fail(
              "Cannot start OpenSeal. Run make build, then restart the development server.",
            ),
          );
          process.once("exit", () => {
            if (!closing)
              fail(
                "OpenSeal stopped. Save provider settings or restart the development server to reconnect.",
              );
          });
          createInterface({ input: process.stdout }).on("line", (line) => {
            if (!line.startsWith("OPENSEAL_DAEMON ")) return;
            try {
              const result = JSON.parse(line.slice(16));
              const url = new URL(result.endpoint);
              if (
                result.type !== "ready" ||
                url.hostname !== "127.0.0.1" ||
                !url.port ||
                url.protocol !== "http:"
              )
                throw new Error();
              endpoint = url.origin;
              error = "";
              clearTimeout(timeout);
              ready();
            } catch {
              fail("OpenSeal returned an invalid startup response.");
            }
          });
        });
      }
      async function stop() {
        const owned = child;
        child = undefined;
        endpoint = "";
        if (!owned || owned.exitCode !== null || owned.signalCode !== null)
          return;
        await new Promise<void>((done) => {
          const timer = setTimeout(() => owned.kill("SIGKILL"), 5000);
          owned.once("exit", () => {
            clearTimeout(timer);
            done();
          });
          owned.kill("SIGTERM");
        });
      }
      void start().catch(() => {});
      server.httpServer?.once("close", () => {
        closing = true;
        void stop();
      });
      const onExit = () => {
        child?.kill("SIGTERM");
      };
      process.once("exit", onExit);
      server.middlewares.use(async (req, res, next) => {
        if (
          !req.url?.startsWith("/__desktop/") &&
          !req.url?.startsWith("/api/v1/")
        )
          return next();
        const origin = req.headers.origin;
        if (
          (origin && origin !== `http://${req.headers.host}`) ||
          req.headers["sec-fetch-site"] === "cross-site"
        ) {
          res.writeHead(403).end();
          return;
        }
        res.setHeader("Content-Type", "application/json");
        res.setHeader("Cache-Control", "no-store");
        if (req.url === "/__desktop/status") {
          res.end(
            JSON.stringify({
              state: error ? "error" : endpoint ? "ready" : "starting",
              message: error,
              workspace: directory,
            }),
          );
          return;
        }
        if (req.url === "/__desktop/provider") {
          if (req.method === "GET") {
            try {
              res.end(
                JSON.stringify(
                  await providerSettings(directory, { action: "load" }),
                ),
              );
            } catch (e) {
              res
                .writeHead(400)
                .end(JSON.stringify({ error: (e as Error).message }));
            }
            return;
          }
          if (
            req.method !== "POST" ||
            saving ||
            !req.headers["content-type"]?.startsWith("application/json")
          ) {
            res.writeHead(saving ? 409 : 400).end(
              JSON.stringify({
                error: saving
                  ? "A provider update is already in progress."
                  : "Invalid provider settings request.",
              }),
            );
            return;
          }
          saving = true;
          try {
            const settings = await providerSettings(directory, {
              action: "save",
              settings: JSON.parse((await requestBody(req)).toString()),
            });
            await stop();
            let reconnected = true;
            try {
              await start();
            } catch {
              reconnected = false;
            }
            res.end(
              JSON.stringify({
                settings,
                reconnected,
                message: reconnected
                  ? "Provider settings saved. Workspace reconnected."
                  : "Settings were saved, but OpenSeal could not restart. Check the configuration and save again.",
              }),
            );
          } catch (e) {
            res.writeHead(400).end(
              JSON.stringify({
                error:
                  e instanceof SyntaxError
                    ? "Invalid provider settings."
                    : (e as Error).message,
              }),
            );
          } finally {
            saving = false;
          }
          return;
        }
        if (req.url === "/__desktop/skill-credential") {
          if (
            req.method !== "POST" ||
            saving ||
            !req.headers["content-type"]?.startsWith("application/json")
          ) {
            res
              .writeHead(saving ? 409 : 400)
              .end(
                JSON.stringify({
                  error: saving
                    ? "A workspace update is already in progress."
                    : "Invalid Skill connection request.",
                }),
              );
            return;
          }
          saving = true;
          try {
            const credential = await providerSettings(directory, {
              action: "save_skill_credential",
              credential: JSON.parse((await requestBody(req)).toString()),
            });
            await stop();
            let reconnected = true;
            try {
              await start();
            } catch {
              reconnected = false;
            }
            res.end(
              JSON.stringify({
                credential,
                reconnected,
                message: reconnected
                  ? "Skill connection saved. Workspace reconnected."
                  : "Skill connection was saved, but the workspace could not restart. Check the configuration and reopen OpenSeal.",
              }),
            );
          } catch (e) {
            res
              .writeHead(400)
              .end(
                JSON.stringify({
                  error:
                    e instanceof SyntaxError
                      ? "Invalid Skill connection request."
                      : (e as Error).message,
                }),
              );
          } finally {
            saving = false;
          }
          return;
        }
        if (!endpoint || error || saving) {
          res
            .writeHead(503)
            .end(
              JSON.stringify({ error: error || "OpenSeal is reconnecting." }),
            );
          return;
        }
        try {
          const target = new URL(req.url, endpoint);
          if (
            target.origin !== endpoint ||
            !target.pathname.startsWith("/api/v1/")
          ) {
            res.writeHead(400).end();
            return;
          }
          const body = await requestBody(req);
          const response = await fetch(target, {
            method: req.method,
            headers: {
              Authorization: `Bearer ${token}`,
              "Content-Type": "application/json",
              ...(req.headers["idempotency-key"]
                ? { "Idempotency-Key": String(req.headers["idempotency-key"]) }
                : {}),
            },
            body: ["GET", "HEAD"].includes(req.method || "GET")
              ? undefined
              : body,
            redirect: "error",
            signal: AbortSignal.timeout(30_000),
          });
          const chunks: Uint8Array[] = [];
          let length = 0;
          const limit = target.pathname.endsWith("/content")
            ? 100 * 1024 * 1024
            : 16 * 1024 * 1024;
          if (response.body) {
            const reader = response.body.getReader();
            try {
              while (true) {
                const { done, value } = await reader.read();
                if (done) break;
                length += value.byteLength;
                if (length > limit) throw new Error("Response too large");
                chunks.push(value);
              }
            } finally {
              await reader.cancel();
            }
          }
          res.setHeader(
            "Content-Type",
            response.headers.get("content-type") || "application/octet-stream",
          );
          res.writeHead(response.status).end(Buffer.concat(chunks));
        } catch {
          res.writeHead(502).end(
            JSON.stringify({
              error:
                "Cannot reach OpenSeal. Check the work status before retrying.",
            }),
          );
        }
      });
    },
  };
}
export default defineConfig(({ command, mode }) => ({
  plugins: [
    react(),
    ...(command === "serve" && mode !== "native" ? [localDaemon()] : []),
  ],
  server: { port: 1420, strictPort: true },
  clearScreen: false,
}));
