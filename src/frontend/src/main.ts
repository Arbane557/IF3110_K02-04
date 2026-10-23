import "./style.css";

const app = document.querySelector<HTMLDivElement>("#app");

if (!app) {
  throw new Error("missing #app");
}

app.innerHTML = `
  <h1>Auth tester</h1>

  <form id="login-form">
    <label>
      Username
      <input id="username" name="username" autocomplete="username" required>
    </label>

    <label>
      Password
      <input
        id="password"
        name="password"
        type="password"
        autocomplete="current-password"
        required
      >
    </label>

    <button type="submit">Login</button>
  </form>

  <div class="actions">
    <button id="whoami" type="button">Who am I?</button>
    <button id="logout" type="button">Logout</button>
  </div>

  <pre id="result">Ready.</pre>
`;

const loginForm = document.querySelector<HTMLFormElement>("#login-form")!;
const whoamiButton = document.querySelector<HTMLButtonElement>("#whoami")!;
const logoutButton = document.querySelector<HTMLButtonElement>("#logout")!;
const result = document.querySelector<HTMLPreElement>("#result")!;

async function request(path: string, options: RequestInit = {}): Promise<void> {
  result.textContent = "Loading...";

  try {
    const response = await fetch(path, {
      ...options,
      credentials: "include",
    });

    const body = await response.text();
    let formattedBody = body || "(empty response)";

    try {
      formattedBody = JSON.stringify(JSON.parse(body), null, 2);
    } catch {
      // Keep non-JSON responses as plain text.
    }

    result.textContent = `${response.status} ${response.statusText}\n${formattedBody}`;
  } catch (error) {
    result.textContent = error instanceof Error ? error.message : String(error);
  }
}

loginForm.addEventListener("submit", async (event) => {
  event.preventDefault();

  const formData = new FormData(loginForm);

  await request("/login", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      username: formData.get("username"),
      password: formData.get("password"),
    }),
  });
});

whoamiButton.addEventListener("click", async () => {
  await request("/whoami");
});

logoutButton.addEventListener("click", async () => {
  await request("/logout", {
    method: "POST",
  });
});
