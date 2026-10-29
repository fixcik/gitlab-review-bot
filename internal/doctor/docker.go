package doctor

import (
	"context"
	"os/exec"
	"strings"
)

// Exec runs a command and returns its combined output.
type Exec func(ctx context.Context, name string, args ...string) ([]byte, error)

// DockerInput is what the Docker checks need.
type DockerInput struct {
	Image, Network string
	// Exec runs docker (nil = os/exec); GOOS picks the hints (linux needs
	// DOCKER_GID, darwin Docker Desktop).
	Exec Exec
	GOOS string
}

func osExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Docker checks the daemon, the sandbox image and the sandbox network.
func Docker(in DockerInput) Provider {
	return ProviderFunc(func(ctx context.Context) []Result {
		if in.Exec == nil {
			in.Exec = osExec
		}
		acc := Result{ID: "docker.access", Group: "Docker", Title: "доступ к Docker"}
		img := Result{ID: "docker.sandbox_image", Group: "Docker", Title: "образ песочницы"}
		netw := Result{ID: "docker.sandbox_network", Group: "Docker", Title: "сеть песочницы"}
		out, err := in.Exec(ctx, "docker", "version", "--format", "{{.Server.Version}}")
		if err != nil {
			acc.Status, acc.Detail = Fail, "нет доступа к Docker: "+oneLine(string(out))
			if in.GOOS == "linux" {
				acc.Action = "в контейнере бота: DOCKER_GID в .env = группа /var/run/docker.sock (`stat -c %g /var/run/docker.sock`); на хосте — пользователь в группе docker"
			} else {
				acc.Action = "запустите Docker Desktop"
			}
			img.Status, netw.Status = Skip, Skip
			return []Result{acc, img, netw}
		}
		acc.Status, acc.Detail = OK, "Docker "+strings.TrimSpace(string(out))
		if _, err := in.Exec(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", in.Image); err == nil {
			img.Status, img.Detail = OK, in.Image
		} else {
			img.Status = Fail
			out, err := in.Exec(ctx, "docker", "manifest", "inspect", in.Image)
			switch {
			case err == nil:
				img.Detail, img.Action = in.Image+" не скачан", "docker compose pull (или собрать: docker compose build)"
			case strings.Contains(strings.ToLower(string(out)), "unauthorized") || strings.Contains(strings.ToLower(string(out)), "denied"):
				img.Detail = in.Image + " не скачан: реестр отказал (unauthorized) — пакет приватный или ещё не опубликован"
				img.Action = "приватный пакет ghcr.io: docker login ghcr.io -u <пользователь GitHub> с PAT read:packages (или сделайте пакет публичным), затем docker compose pull; не опубликован (нет тега) — соберите: docker compose build"
			default:
				img.Detail, img.Action = in.Image+" не найден ни локально, ни в реестре: "+oneLine(string(out)), "соберите: docker compose build (или проверьте имя образа sandbox.image)"
			}
		}
		if _, err := in.Exec(ctx, "docker", "network", "inspect", in.Network); err == nil {
			netw.Status, netw.Detail = OK, in.Network
		} else {
			netw.Status, netw.Detail, netw.Action = Warn, in.Network+" не создана", "её создаёт docker compose up -d"
		}
		return []Result{acc, img, netw}
	})
}
