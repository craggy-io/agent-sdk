def run(step, max_steps):
    for _ in range(max_steps):
        status, value = step()
        if status == "actions":
            status = value()
        if status != "continue":
            return
